// Package main is the entry point for audit-event-store-svc.
//
// This service is an append-only evidence store for domain events
// (see docs/architecture/03-microservices.md §14.1).  It has no
// outward-facing business HTTP API; the HTTP server here exists solely
// to serve health probes so Kubernetes knows the process is alive and
// the DB is reachable.  The Kafka consumer loop is a separate task and
// is NOT wired here yet.
//
// Wiring order:
//
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — fail-fast Ping on startup
//  4. Construct PgStore
//  5. Mount health probes on chi router
//  6. Start HTTP server with graceful shutdown on SIGTERM/SIGINT
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/config"
	"zoiko.io/audit-event-store-svc/internal/health"
	"zoiko.io/audit-event-store-svc/internal/store"
)

func main() {
	// ── 1. Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	// ── 2. Logger ────────────────────────────────────────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("audit-event-store-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("db_name", cfg.DB.Name),
	)

	// ── 3. Database pool ─────────────────────────────────────────────────────
	// Explicit pool configuration matching the other Tier 0 services.
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		log.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatal("failed to create db pool", zap.Error(err))
	}
	defer pool.Close()

	// Fail-fast connectivity check — DB is a hard Tier 0 dependency.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Store ─────────────────────────────────────────────────────────────
	// PgStore is constructed here and will be passed to the Kafka consumer
	// goroutine once that task is implemented.  For now it is unused at
	// runtime but its construction validates the DB connection is usable.
	_ = store.NewPgStore(pool, log)

	// ── 5. HTTP router ────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(correlationIDMiddleware)

	// Health probes — no auth, no tenant context required.
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", healthH.Readiness)

	// ── 6. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + itoa(cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		log.Fatal("server error", zap.Error(err))
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("server stopped")
}

// correlationIDMiddleware propagates X-Correlation-ID through every request.
// If the header is absent a new ID is injected via chi's RequestID middleware.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}

// itoa converts a non-negative int to its decimal string representation.
func itoa(i int) string {
	if i == 0 {
		return "8080"
	}
	b := make([]byte, 0, 5)
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
