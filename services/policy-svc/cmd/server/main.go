// Package main is the entry point for policy-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore
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

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/cache"
	"zoiko.io/policy-svc/internal/config"
	"zoiko.io/policy-svc/internal/consumer"
	"zoiko.io/policy-svc/internal/decisionlog"
	svcenvelope "zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/evidence"
	"zoiko.io/policy-svc/internal/handler"
	"zoiko.io/policy-svc/internal/health"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
	"zoiko.io/policy-svc/internal/mtlslistener"
	"zoiko.io/policy-svc/internal/outbox"
	"zoiko.io/policy-svc/internal/scheduler"
	"zoiko.io/policy-svc/internal/store"
	"zoiko.io/policy-svc/internal/telemetry"
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

	log.Info("policy-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("governance_decision_log_url", cfg.GovernanceDecisionLogServiceURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "policy-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("policy-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as jurisdiction-rules-svc/
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

	// ── 3b. Kafka producer & outbox worker ────────────────────────────────────
	kafkaWriter := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Kafka.Brokers...),
		Topic:        cfg.Kafka.Topic,
		Balancer:     &kafka.LeastBytes{},
		BatchSize:    100,
		BatchTimeout: 10 * time.Millisecond,
		RequiredAcks: kafka.RequireAll,
		Async:        false,
	}
	defer func() {
		if err := kafkaWriter.Close(); err != nil {
			log.Error("kafka writer close failed", zap.Error(err))
		}
	}()

	// Every state change writes its events to the outbox in its own
	// transaction; this relay publishes them after commit.
	outboxWorker := outbox.NewWorker(pool, kafkaWriter, log).WithResidency(cfg.EventResidencyRegion)
	outboxWorker.Start()
	defer outboxWorker.Stop()

	// ── 4. Store, resolution cache, background workers ────────────────────────
	pgStore := store.New(pool, log)
	resolutionCache := cache.New(pgStore)

	// Evaluation evidence is stored before Evaluate answers; this relay
	// delivers it to the governance ledger as policy-svc's own principal.
	if cfg.LedgerPrincipalID == "" {
		log.Error("POLICY_LEDGER_PRINCIPAL_ID is not set: evaluations will be stored but stay PENDING for the governance ledger")
	}
	evidenceRelay := evidence.NewRelay(pgStore, decisionlog.NewHTTPClient(cfg.GovernanceDecisionLogServiceURL),
		cfg.LedgerPrincipalID, cfg.EvidenceRelayInterval, log)
	evidenceRelay.Start()
	defer evidenceRelay.Stop()

	activationScheduler := scheduler.New(pgStore, cfg.SchedulerInterval, log)
	activationScheduler.Start()
	defer activationScheduler.Stop()

	stopBacklog := watchBacklogs(pgStore, metrics, log)
	defer stopBacklog()

	// ── 4b. Kafka consumer for external events ────────────────────────────────
	consumerCfg := consumer.DefaultConsumerConfig(cfg.Kafka.Brokers, cfg.Kafka.GroupID)
	consumerHandler := consumer.NewHandler(resolutionCache, log)
	eventConsumer := consumer.NewConsumer(consumerCfg, consumerHandler, log)
	eventConsumer.Start(context.Background())
	defer eventConsumer.Stop()

	// AuthZ client. Refuses to start in production/staging against a
	// placeholder URL — no service may silently fall back to permit-all.
	authzClient, err := authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
	if err != nil {
		log.Fatal("authz client construction failed", zap.Error(err))
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(otelchi.Middleware("policy-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(chimiddleware.Logger)

	// Identity before anything reads X-Tenant-Id / X-Principal-Id (X5), through
	// the platform's shared gate (decision 21 Oct 2026). policy-svc's own
	// verifier checked the envelope's signature and nothing else, so a session
	// ended at identity-context-svc (logout, suspension) kept working here until
	// the envelope expired (AU-01). The gate also checks the session status, and
	// makes the verified claims the only source of the identity headers.
	gate, gateSettings, err := idenv.NewGateFromEnv("policy-svc", log, gateOptions(cfg))
	if err != nil {
		log.Fatal("identity gate configuration refused", zap.Error(err))
	}
	log.Info("identity gate", gateSettings.Fields()...)
	r.Use(gate.Middleware)
	r.Use(svcmiddleware.TenantContext())

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// Evaluate is exempt from MaterialWrite, as authorization-svc's
	// /v1/authorize is (audit C5): its replay protection is the caller's
	// decision_id, which reconciles a retry to the stored result, so an
	// Idempotency-Key would be a second key for the same thing.
	envelopePolicy := svcenvelope.ServicePolicy()
	envelopePolicy.MaterialWrite = func(req *http.Request) bool {
		if req.URL.Path == "/v1/policies/evaluate" && req.Method == http.MethodPost {
			return false
		}
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return false
		default:
			return true
		}
	}
	// LegalEntityID is not universally required: a policy family and a global
	// or tenant-wide version have no legal entity. Where a body names one, the
	// handlers reconcile it with X-Legal-Entity-Id (a mismatch is 400).
	envelopePolicy.LegalEntityID = svcenvelope.NotRequired
	// Refusals rendered as RFC 9457 problem+json, like every other error here.
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.Middleware(envelopePolicy, svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, resolutionCache, authzClient, handler.Options{
		PlatformScopeID: cfg.AuthZPlatformScopeID,
		EvaluateAuthz:   handler.ParseMode(cfg.EvaluateAuthz),
		ReadAuthz:       handler.ParseMode(cfg.ReadAuthz),
	}, log)
	if handler.ParseMode(cfg.EvaluateAuthz) == handler.ModeWarn || handler.ParseMode(cfg.ReadAuthz) == handler.ModeWarn {
		log.Warn("policy-svc authorization gates in WARN mode: refusals are logged, not enforced",
			zap.String("evaluate", cfg.EvaluateAuthz), zap.String("read", cfg.ReadAuthz))
	}
	handler.RegisterRoutes(r, h)
	handler.RegisterControlTestRoutes(r, h)

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

	// Mutual-TLS listener for peer services (SEC-INV-05): same router,
	// provisioned in the background so an unavailable mtls-management-svc
	// never stops the plaintext listener.
	mtlsCtx, stopMTLS := context.WithCancel(context.Background())
	defer stopMTLS()
	var mtlsListener *mtlslistener.Listener
	if cfg.MTLSEnabled {
		mtlsListener = mtlslistener.Start(mtlsCtx, mtlslistener.Options{
			ServiceName:        "policy-svc",
			Port:               cfg.MTLSPort,
			ManagementURL:      cfg.MTLSManagementURL,
			BootstrapTokenPath: cfg.MTLSBootstrapTokenPath,
			AllowedClients:     cfg.MTLSAllowedClients,
		}, r, log)
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
	if mtlsListener != nil {
		stopMTLS()
		if err := mtlsListener.Shutdown(shutdownCtx); err != nil {
			log.Error("mtls listener shutdown failed", zap.Error(err))
		}
	}
	log.Info("server stopped")
}

// watchBacklogs exports both outboxes' backlogs every 30 seconds. Returns a
// stop function.
func watchBacklogs(st *store.PgStore, m *telemetry.Metrics, log *zap.Logger) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if pending, failed, err := st.EvidenceBacklog(ctx); err == nil {
				m.EvidencePending.Set(float64(pending))
				m.EvidenceFailed.Set(float64(failed))
			} else {
				log.Warn("evidence backlog unreadable", zap.Error(err))
			}
			if pending, oldest, err := st.OutboxBacklog(ctx); err == nil {
				m.OutboxPending.Set(float64(pending))
				m.OutboxOldestSeconds.Set(oldest)
			} else {
				log.Warn("outbox backlog unreadable", zap.Error(err))
			}
			cancel()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(stop); <-done }
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
