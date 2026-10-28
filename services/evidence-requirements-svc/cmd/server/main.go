// Package main is the entry point for evidence-requirements-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore, Kafka producer, authz client, document-vault client
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
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/evidence-requirements-svc/internal/authz"
	"zoiko.io/evidence-requirements-svc/internal/config"
	"zoiko.io/evidence-requirements-svc/internal/documentvault"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	"zoiko.io/evidence-requirements-svc/internal/health"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/mtls"
	"zoiko.io/evidence-requirements-svc/internal/outbox"
	"zoiko.io/evidence-requirements-svc/internal/registry"
	"zoiko.io/evidence-requirements-svc/internal/store"
	"zoiko.io/evidence-requirements-svc/internal/telemetry"
)

// platformScopeID is the platform-wide legal entity scope used when
// provisioning this service's own mTLS client identity from
// mtls-management-svc (see internal/mtls).
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

	log.Info("evidence-requirements-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.String("document_vault_url", cfg.DocumentVaultServiceURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "evidence-requirements-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("evidence-requirements-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as policy-svc/jurisdiction-rules-svc/
	// purchase-order-svc.
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

	// ── 4. Store, Kafka producer, authz + document-vault clients ─────────────
	pgStore := store.New(pool, log)

	// Kafka producer — connects lazily on first write, same posture as every
	// other producer here: not a fail-fast startup dependency like Postgres.
	//
	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its metadata
	// request, so every write to a not-yet-existing topic fails with
	// "Unknown Topic Or Partition" — logged only, never surfaced. As of
	// 2026-07-27, 31 of the 42 services with a Kafka producer still omit this
	// field and have therefore likely never published a single event. Set
	// here so this service's events actually land; see project memory
	// zoikosuite-known-issues for the platform-wide finding.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
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
	defer func() { _ = kafkaWriter.Close() }()

	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "evidence-requirements-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewHTTPClientWithHTTPClient(cfg.AuthzMTLSURL, log, mtlsHTTPClient)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}
	docsClient := documentvault.NewHTTPClient(cfg.DocumentVaultServiceURL, log)

	// ── 4b. Outbox worker ──────────────────────────────────────────────────────
	outboxWorker := outbox.NewWorker(pool, kafkaWriter, log)
	outboxWorker.Start(context.Background())
	defer outboxWorker.Stop()

	// ── 4c. Identity gate and legal-entity registry ──────────────────────────
	// X-Tenant-Id / X-Principal-Id were trusted as sent while port 8130 was
	// published (Governance audit X5). The gate verifies identity-context-
	// svc's signed envelope and that its session is still live (AU-01), then
	// sets those headers from the verified claims. IDENTITY_VERIFY: require
	// (default outside local) | warn.
	gate, gateSettings, err := idenv.NewGateFromEnv("evidence-requirements-svc", log, idenv.EnvOptions{})
	if err != nil {
		log.Fatal("identity gate configuration refused", zap.Error(err))
	}
	log.Info("identity gate", gateSettings.Fields()...)

	var entityChecker handler.EntityChecker
	switch {
	case cfg.TenantRegistryURL != "":
		entityChecker = registry.New(cfg.TenantRegistryURL, log)
	case !cfg.LocalEnv():
		log.Fatal("TENANT_REGISTRY_URL is required outside local/dev: the legal entity of every evaluation and requirement is server-validated")
	default:
		log.Warn("TENANT_REGISTRY_URL unset — legal entities are not confirmed against the registry (local/dev only)")
		entityChecker = registry.Noop{}
	}
	evaluateMode := handler.ParseGateMode(cfg.EvaluateAuthzMode)
	readMode := handler.ParseGateMode(cfg.ReadAuthzMode)
	if (evaluateMode == handler.GateWarn || readMode == handler.GateWarn) && !cfg.LocalEnv() {
		log.Warn("evidence authorization gate in WARN mode outside local/dev — refusals are logged and admitted",
			zap.String("evaluate", string(evaluateMode)), zap.String("read", string(readMode)))
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(otelchi.Middleware("evidence-requirements-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(gate.Middleware)
	r.Use(correlationIDMiddleware)
	// Reads the caller's tenant scope from X-Tenant-Id (set by
	// gateway-auth-svc's ForwardAuth verification) into context, so every DB
	// call can filter by it explicitly — see internal/store's doc comment on
	// why RLS alone is not sufficient here.
	r.Use(svcmiddleware.TenantContext())
	r.Use(chimiddleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	// Refusals rendered as RFC 9457 problem+json, like every other error here.
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, authzClient, docsClient, log).
		WithRegistry(entityChecker).
		WithGateModes(evaluateMode, readMode)
	handler.RegisterRoutes(r, h)
	handler.RegisterPBCRoutes(r, h, pgStore)

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
			r.Header.Set("X-Correlation-ID", chimiddleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
