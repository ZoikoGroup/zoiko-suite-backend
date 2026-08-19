// Package main is the entry point for jurisdiction-rules-svc.
//
// Wiring order:
//  1. Load config from environment (fails fast on unsafe combinations)
//  2. Initialise structured logger (zap)
//  3. Tracing + metrics
//  4. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  5. Construct PgStore, AuthZ client, event publisher
//  6. Construct HTTP handler + mount routes on chi router
//  7. Mount health probes (/healthz, /readyz, /metrics)
//  8. Start HTTP server with graceful shutdown
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/config"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/health"
	"zoiko.io/jurisdiction-rules-svc/internal/mtls"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
	"zoiko.io/jurisdiction-rules-svc/internal/telemetry"
)

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	// ── 2. Logger ─────────────────────────────────────────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("jurisdiction-rules-svc starting",
		zap.String("env", cfg.Env),
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.Strings("kafka_brokers", cfg.Kafka.Brokers),
	)

	// ── 3. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "jurisdiction-rules-svc", cfg.OTELExporterEndpoint)
	if err != nil {
		log.Fatal("otel tracing init failed", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Error("otel tracer provider shutdown failed", zap.Error(err))
		}
	}()

	metrics := telemetry.NewMetrics("jurisdiction-rules-svc")

	// ── 4. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as tenant-entity-registry-svc.
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		log.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
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

	// Verify connectivity at startup — fail fast rather than silently degrade.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 5. Store, AuthZ client, event publisher ───────────────────────────────
	pgStore := store.New(pool, log)

	// AuthZ client. Fails fast at startup if ENV is production/staging and
	// AuthZServiceURL is still empty or a dev placeholder — no domain
	// service may silently fall back to a permit-all stub in production.
	var authzClient authz.AuthorizationClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "jurisdiction-rules-svc", cfg.AuthZPlatformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewHTTPAuthZClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, log)
	} else {
		authzClient, err = authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
		if err != nil {
			log.Fatal("authz client construction failed", zap.Error(err))
		}
	}

	publisher, closeProducer := newPublisher(cfg, log)
	defer closeProducer()

	// ── 6. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("jurisdiction-rules-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(middleware.Logger)

	h := handler.New(pgStore, authzClient, publisher, cfg.AuthZPlatformScopeID, log)
	handler.RegisterRoutes(r, h)

	// ── 7. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 8. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + strconv.Itoa(cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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

// newPublisher builds the event publisher and its cleanup function.
//
// Kafka connects lazily on first write, so it is not a fail-fast startup
// dependency like Postgres — same posture as identity-context-svc,
// tenant-entity-registry-svc, policy-svc and obligations-svc. With no
// brokers configured the service still runs, dropping events, which keeps
// single-service local development to two containers; that fallback is
// refused outside local development, because a production deployment
// silently publishing nothing is exactly the failure the events exist to
// prevent.
func newPublisher(cfg *config.Config, log *zap.Logger) (events.Publisher, func()) {
	if len(cfg.Kafka.Brokers) == 0 {
		if strings.EqualFold(cfg.Env, "production") || strings.EqualFold(cfg.Env, "staging") {
			log.Fatal("KAFKA_BROKERS must be set in " + cfg.Env + " environment")
		}
		log.Warn("no Kafka brokers configured — domain events will be dropped")
		return events.NewNoopPublisher(log), func() {}
	}

	writer := &kafka.Writer{
		Addr:     kafka.TCP(cfg.Kafka.Brokers...),
		Topic:    cfg.Kafka.Topic,
		Balancer: &kafka.LeastBytes{},
		// Required even though the broker sets auto.create.topics.enable:
		// kafka-go defaults this to false and never asks the broker to
		// auto-create in its metadata request, so a write to a topic that does
		// not exist yet fails with "Unknown Topic Or Partition" regardless of
		// the broker-side setting. Matches the platform-wide fix in 7589bc3.
		AllowAutoTopicCreation: true,
		// Bounded so an unreachable broker delays a mutation's response
		// rather than holding the request open. The write has already been
		// committed by the time an event is emitted; the caller should not
		// wait on the event backbone to hear about it.
		WriteTimeout: 5 * time.Second,
		// Without this, every write to this service costs an extra second.
		// kafka-go batches, and BatchTimeout defaults to 1s: a synchronous
		// WriteMessages of a single message waits for the batch to fill (100
		// messages) or for that timer, whichever comes first. These events are
		// emitted one per state transition, so the batch never fills and the
		// timer always wins — and publishing is on the request path, so the
		// caller pays for it. Ordering and synchronous delivery are unchanged;
		// only the artificial wait goes away.
		BatchTimeout: 10 * time.Millisecond,
	}
	return events.NewPublisher(log, cfg.Kafka.Topic, writer), func() { _ = writer.Close() }
}

// correlationIDMiddleware propagates X-Correlation-ID through every request.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
