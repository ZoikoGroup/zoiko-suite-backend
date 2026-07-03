// Package main is the entry point for tenant-entity-registry-svc.
//
// Wiring order:
//   1. Load config from environment
//   2. Initialise structured logger (zap)
//   3. Connect to PostgreSQL pool (pgxpool)
//   4. Construct dependency implementations: store, events publisher, authz client, jurisdiction validator
//   5. Construct registry.Service
//   6. Construct HTTP handler + mount routes on chi router
//   7. Mount health probes on a separate internal router
//   8. Start HTTP server with graceful shutdown
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

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/config"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/handler"
	"zoiko.io/tenant-entity-registry-svc/internal/health"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
	svcmiddleware "zoiko.io/tenant-entity-registry-svc/internal/middleware"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

func main() {
	// ── 1. Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		// Can't log yet; write to stderr and exit.
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

	log.Info("tenant-entity-registry-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("jurisdiction_rules_url", cfg.JurisdictionRulesURL),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 3. Database pool ─────────────────────────────────────────────────────
	// F8: explicit pool configuration for a Tier 0 service.
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

	// Verify connectivity at startup.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Dependencies ──────────────────────────────────────────────────────

	pgStore := store.New(pool, log)

	eventPublisher := events.NewPublisher(log, cfg.Kafka.Topic)

	// Authorization client.
	// Switch to authz.NewHTTPAuthZClient(cfg.AuthZServiceURL, log) before
	// Phase 1 production cutover per doctrine: no service self-authorizes.
	var authzClient authz.AuthorizationClient = authz.NewStubAuthZClient(log)
	if cfg.AuthZServiceURL != "" && cfg.AuthZServiceURL != "http://authorization-svc" {
		authzClient = authz.NewHTTPAuthZClient(cfg.AuthZServiceURL, log)
		log.Info("using HTTP authorization client", zap.String("url", cfg.AuthZServiceURL))
	} else {
		log.Warn("using STUB authorization client — wire real AuthZ before production")
	}

	// Jurisdiction validator.
	// Switch to jurisdiction.NewHTTPValidator when the Jurisdiction Rules Service ships.
	var jurisdValidator jurisdiction.JurisdictionValidator = jurisdiction.NewStubValidator(log)
	if cfg.JurisdictionRulesURL != "" && cfg.JurisdictionRulesURL != "http://jurisdiction-rules-svc" {
		jurisdValidator = jurisdiction.NewHTTPValidator(cfg.JurisdictionRulesURL, log)
		log.Info("using HTTP jurisdiction validator", zap.String("url", cfg.JurisdictionRulesURL))
	} else {
		log.Warn("using STUB jurisdiction validator — wire real service before production")
	}

	// ── 5. Service ───────────────────────────────────────────────────────────
	svc := registry.NewService(pgStore, eventPublisher, authzClient, jurisdValidator, log)

	// ── 6. HTTP router ───────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(correlationIDMiddleware)
	// F1: extract tenant_id from JWT into context so every DB call can set
	// app.tenant_id on the Postgres session and RLS is actually enforced.
	r.Use(svcmiddleware.TenantContext(log))
	r.Use(middleware.Logger)

	h := handler.New(svc, log)
	handler.RegisterRoutes(r, h)

	// ── 7. Health probes (separate path, no auth) ────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", healthH.Readiness)

	// ── 8. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":8081"
	if cfg.Port != 0 {
		addr = ":" + itoa(cfg.Port)
	}
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Run server in a goroutine so we can listen for shutdown signals.
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Wait for SIGINT or SIGTERM.
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

func itoa(i int) string {
	if i == 0 {
		return "8081"
	}
	b := make([]byte, 0, 5)
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
