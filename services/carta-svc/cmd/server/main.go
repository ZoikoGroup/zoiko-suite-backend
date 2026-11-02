package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/carta-svc/internal/authz"
	"zoiko.io/carta-svc/internal/config"
	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/idempotency"
	"zoiko.io/carta-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	// Config fails on a missing DATABASE_URL or AUTHZ_SERVICE_URL: the
	// assessments are authoritative decision records and live only in
	// Postgres, so there is no in-memory fallback to start with.
	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("invalid configuration", zap.Error(err))
	}
	port := cfg.Port

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("failed to parse DATABASE_URL", zap.Error(err))
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	// The pool connects lazily, so a database that is down at boot does not
	// crash-loop the process; /readyz reports it until it comes back.
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		logger.Fatal("failed to create database pool", zap.Error(err))
	}
	defer pool.Close()

	s := store.NewPgStore(pool)
	// Fail fast rather than starting and 503-ing every guarded route.
	// authz.NewClient("") builds requests against an empty base URL, so every
	// CheckAllowed fails and the client — correctly — refuses. That posture is
	// right at request time but wrong at boot: a service that starts and then
	// rejects all authorized traffic hides a deployment mistake until a user
	// finds it. This dependency was added in Priority 2b and was missing from
	// docker-compose.yml for two services, which is exactly how that happens.
	// (config.Load enforces it.)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	h := handler.New(s, authzClient, logger).
		WithIdempotency(idempotency.NewPgStore(pool)).
		WithReadiness(pool)
	router := handler.NewRouter(h)

	// Wrap router with apicontract middleware
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Mount("/", router)

	srv := &http.Server{Addr: ":" + port, Handler: r, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("carta-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
