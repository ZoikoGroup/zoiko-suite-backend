// Command server runs the public-ai-svc HTTP API.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer log.Sync() //nolint:errcheck

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("invalid configuration", zap.Error(err))
	}

	st := openStore(log, cfg)
	api := httpapi.New(cfg, log, httpapi.Deps{
		Store:    st,
		Sessions: session.New(st, cfg.SessionTTL),
		Answerer: orchestrator.AbstainAll{ReleaseID: "none"},
		Ready:    func() bool { return cfg.Enabled },
	})
	serve(log, cfg, api.Router())
}

func openStore(log *zap.Logger, cfg config.Config) store.Store {
	if cfg.DatabaseURL == "" {
		log.Warn("PUBLIC_AI_DATABASE_URL not set: using in-memory store (local development only)")
		return store.NewMemory()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("database config", zap.Error(err))
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatal("database ping", zap.Error(err))
	}
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		log.Fatal("migrations", zap.Error(err))
	}
	return store.NewPostgres(pool)
}

func serve(log *zap.Logger, cfg config.Config, h http.Handler) {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Info("public-ai-svc listening", zap.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server failed", zap.Error(err))
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("forced shutdown", zap.Error(err))
	}
}
