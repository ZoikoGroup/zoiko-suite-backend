// Package main is the entry point for accounts-payable-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore, Kafka producer, jurisdiction-rules-svc validator
//  5. Construct HTTP handler + mount routes on chi router
//  6. Mount health probes (/healthz, /readyz)
//  7. Start HTTP server with graceful shutdown
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	"zoiko.io/accounts-payable-svc/internal/authz"
	"zoiko.io/accounts-payable-svc/internal/config"
	"zoiko.io/accounts-payable-svc/internal/events"
	"zoiko.io/accounts-payable-svc/internal/handler"
	"zoiko.io/accounts-payable-svc/internal/health"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/mtls"
	"zoiko.io/accounts-payable-svc/internal/payableopenitem"
	"zoiko.io/accounts-payable-svc/internal/store"
	"zoiko.io/accounts-payable-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

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

	log.Info("accounts-payable-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "accounts-payable-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("accounts-payable-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as policy-svc/jurisdiction-rules-svc/
	// tenant-entity-registry-svc.
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

	// ── 4. Store, Kafka producer, jurisdiction validator ─────────────────────
	pgStore := store.New(pool, log)

	// Kafka producer — connects lazily on first write, same posture as
	// identity-context-svc/tenant-entity-registry-svc/policy-svc: not a
	// fail-fast startup dependency like Postgres.
	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	//
	// An explicitly empty KAFKA_BROKERS selects the log-only publisher instead.
	// config.validate permits that for ENV=local and refuses it everywhere else,
	// so a production deployment cannot reach this branch and silently stop
	// publishing.
	var publisher handler.Publisher
	if len(cfg.Kafka.Brokers) == 0 {
		publisher = events.NewLogOnlyPublisher(log)
	} else {
		kafkaWriter := &kafka.Writer{
			Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
			Topic:                  cfg.Kafka.Topic,
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
			// Without this, every write to this service costs an extra second.
			//
			// kafka-go batches, and BatchTimeout defaults to 1s: a synchronous
			// WriteMessages of a single message waits for the batch to fill (100
			// messages) or for that timer to expire, whichever comes first. These
			// events are emitted one per state transition, so the batch never
			// fills and the timer always wins — and because publishing happens on
			// the request path, the caller pays for it. Measured: 2.0s per
			// create, of which ~1s was this timer (the other ~1s is
			// authorization-svc's /v1/authorize, which is not ours to fix here).
			//
			// The events stay ordered and still publish synchronously; only the
			// artificial wait goes away. Async: true would also remove it, but it
			// makes publish failures invisible to the request that caused them.
			BatchTimeout: 10 * time.Millisecond,
		}
		defer func() { _ = kafkaWriter.Close() }()
		publisher = events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	}
	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "accounts-payable-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, log, mtlsHTTPClient)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("accounts-payable-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	// Reads the caller's tenant scope from X-Tenant-Id (set by
	// gateway-auth-svc's ForwardAuth verification) into context, so every DB
	// call can filter by it explicitly — see internal/store's doc comment
	// on why RLS alone is not sufficient here.
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	payablesClient := payableopenitem.NewHTTPClient(cfg.PayableOpenItemServiceURL, log)
	h := handler.New(pgStore, publisher, authzClient, payablesClient, log)
	handler.RegisterRoutes(r, h)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 7. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + strconv.Itoa(cfg.Port)
	// ReadHeaderTimeout is the one that is easy to miss, and the reason all four
	// are stated together. ReadTimeout bounds a whole request, so a client that
	// dribbles a BODY is already cut off -- but a connection that sends a partial
	// HEADER and then stalls holds a goroutine and a descriptor for that entire
	// window without ever becoming a request. Enough of those exhaust the process
	// while every metric still reads healthy.
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
