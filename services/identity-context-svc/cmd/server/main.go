// Command server is the entrypoint for identity-context-svc.
//
// Tier 0 — must be running before any domain or governance service starts.
// See docs/architecture/06-blueprint.md Phase 1 exit criteria.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/health"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/store"
	"zoiko.io/identity-context-svc/internal/telemetry"
	"zoiko.io/identity-context-svc/internal/upstream"
)

func main() {
	// ── Logger (structured JSON, production-grade) ────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	// ── Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}

	// ── Redis client ──────────────────────────────────────────────────────
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// Tier 0 — Redis is a hard dependency. Fail fast on startup.
		log.Fatal("Redis unreachable on startup — aborting", zap.Error(err))
	}
	log.Info("Redis connection established",
		zap.String("addr", fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port)),
	)

	// ── Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "identity-context-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("identity-context-svc")

	// ── Postgres pool ─────────────────────────────────────────────────────
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

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		// Tier 0 — Postgres is a hard dependency. Fail fast on startup.
		log.Fatal("Postgres unreachable on startup — aborting", zap.Error(err))
	}
	log.Info("Postgres connection established", zap.String("db_name", cfg.DB.Name))

	// ── Kafka producer ────────────────────────────────────────────────────
	// Connects lazily on first write — unlike Postgres/Redis this is not a
	// fail-fast startup dependency. Publish failures are handled per-call by
	// the resolver's existing error-return/log path (Gap 1 fix).
	kafkaWriter := &kafka.Writer{
		Addr:     kafka.TCP(cfg.Kafka.Brokers...),
		Topic:    cfg.Kafka.Topic,
		Balancer: &kafka.LeastBytes{},
	}
	defer func() { _ = kafkaWriter.Close() }()

	// ── Domain dependencies ───────────────────────────────────────────────
	sessionCache := session.NewCache(rdb, cfg.Redis.SessionTTLSeconds)
	riskCache := session.NewRiskSignalCache(rdb)
	principalRepo := store.New(pool, log)
	upstreamRegistry := upstream.NewRegistryClient(cfg, log)
	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	verifier := auth.NewJWTVerifier(cfg)
	signer, err := auth.NewJWTSigner(cfg)

	if err != nil {
		log.Fatal("failed to initialise JWT signer", zap.Error(err))
	}

	// ── Resolver ──────────────────────────────────────────────────────────
	resolver := identityctx.NewResolver(
		cfg,
		log,
		principalRepo,
		sessionCache,
		riskCache,
		upstreamRegistry,
		publisher,
		verifier,
		signer,
	)

	// ── HTTP router ───────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Platform middleware
	r.Use(middleware.RequestID)    // injects X-Request-Id
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)    // never let a panic crash the Tier 0 service
	r.Use(otelchi.Middleware("identity-context-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)

	// Structured request logging
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)
			next.ServeHTTP(ww, req)
			log.Info("request",
				zap.String("method", req.Method),
				zap.String("path", req.URL.Path),
				zap.Int("status", ww.Status()),
				zap.Duration("duration", time.Since(start)),
				zap.String("correlation_id", req.Header.Get("X-Correlation-ID")),
			)
		})
	})

	// Health probe (no auth required)
	r.Handle("/health", metrics.WrapReadinessHandler(health.NewHandler(rdb, pool)))
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/.well-known/jwks.json", auth.NewJWKSHandler(signer.PublicKey(), cfg.JWTKeyID))

	// Domain routes (all under /v1/)
	h := identityctx.NewHandler(resolver, sessionCache, principalRepo, log)
	identityctx.RegisterRoutes(r, h)

	// ── Server ────────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info("identity-context-svc starting",
			zap.Int("port", cfg.Port),
			zap.String("tier", "0"),
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining connections")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("draining in-flight event goroutines")
	resolver.Drain()
	log.Info("identity-context-svc stopped")
}
