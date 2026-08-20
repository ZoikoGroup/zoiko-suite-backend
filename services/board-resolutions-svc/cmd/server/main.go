package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/board-resolutions-svc/internal/authz"
	"zoiko.io/board-resolutions-svc/internal/config"
	"zoiko.io/board-resolutions-svc/internal/events"
	"zoiko.io/board-resolutions-svc/internal/evidencereq"
	"zoiko.io/board-resolutions-svc/internal/handler"
	"zoiko.io/board-resolutions-svc/internal/health"
	"zoiko.io/board-resolutions-svc/internal/middleware"
	"zoiko.io/board-resolutions-svc/internal/store"
	"zoiko.io/board-resolutions-svc/internal/telemetry"
)

func main() {
	logger, err := telemetry.NewLogger("board-resolutions-svc")
	if err != nil {
		fmt.Printf("failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		logger.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	// An unreachable database used to be a Warn: the service started anyway,
	// reported itself as up, and answered every request with a store failure
	// until someone read the logs. A service that cannot reach its own
	// database has not started.
	if err != nil {
		logger.Fatal("failed to create db pool", zap.Error(err))
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		logger.Fatal("db unreachable at startup", zap.Error(err))
	}
	logger.Info("connected to postgres database")

	pgStore := store.NewPgStore(pool)
	publisher := events.NewKafkaPublisher(cfg.KafkaBrokers, cfg.KafkaEventsTopic, logger)
	defer func() { _ = publisher.Close() }()
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	evidenceReqClient := evidencereq.NewClient(cfg.EvidenceReqURL)

	h := handler.New(pgStore, publisher, authzClient, evidenceReqClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(correlationIDMiddleware)
	r.Use(middleware.TenantContextMiddleware)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	handler.RegisterRoutes(r, h)

	// ReadHeaderTimeout is the one that is easy to miss, and the reason all four
	// are stated together. ReadTimeout bounds a whole request, so a client that
	// dribbles a BODY is already cut off -- but a connection that sends a partial
	// HEADER and then stalls holds a goroutine and a descriptor for that entire
	// window without ever becoming a request. Enough of those exhaust the process
	// while every metric still reads healthy.
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("starting board-resolutions-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down board-resolutions-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}
	pool.Close()
	logger.Info("server stopped cleanly")
}

// correlationIDMiddleware makes every request carry a correlation id, echoing
// the caller's own when they supply one.
//
// It matters more here than the boilerplate suggests: PassResolution uses
// X-Correlation-ID as the idempotency key for the evidence evaluation, so a
// request that arrives without one takes a fresh-attempt path. Echoing it on
// the response is what lets a caller correlate a refusal with the evaluation
// that produced it.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", chimiddleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
