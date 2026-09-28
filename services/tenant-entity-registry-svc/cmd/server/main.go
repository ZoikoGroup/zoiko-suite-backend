// Package main is the entry point for tenant-entity-registry-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool)
//  4. Construct dependency implementations: store, events publisher, authz client, jurisdiction validator
//  5. Construct registry.Service
//  6. Construct HTTP handler + mount routes on chi router
//  7. Mount health probes on a separate internal router
//  8. Start HTTP server with graceful shutdown
package main

import (
	"context"
	"errors"
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
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/config"
	"zoiko.io/tenant-entity-registry-svc/internal/entitlement"
	svcenvelope "zoiko.io/tenant-entity-registry-svc/internal/envelope"
	"zoiko.io/tenant-entity-registry-svc/internal/handler"
	"zoiko.io/tenant-entity-registry-svc/internal/health"
	"zoiko.io/tenant-entity-registry-svc/internal/idempotency"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
	svcmiddleware "zoiko.io/tenant-entity-registry-svc/internal/middleware"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
	"zoiko.io/tenant-entity-registry-svc/internal/telemetry"
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

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "tenant-entity-registry-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("tenant-entity-registry-svc")

	// ── 3. Database pool ─────────────────────────────────────────────────────
	// F8: explicit pool configuration for a Tier 0 service.
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

	// Verify connectivity at startup.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Dependencies ──────────────────────────────────────────────────────

	pgStore := store.New(pool, log)

	// Kafka producer. Connects lazily on first write — not a fail-fast
	// startup dependency like Postgres. Publish failures are logged inside
	// Publisher.emit(), not propagated (see that method's doc comment).
	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	defer func() { _ = kafkaWriter.Close() }()

	// There is no direct publisher any more: since 28 Sep 2026 every event is
	// written into event_outbox inside its write's transaction and delivered
	// by the relay below — the direct path lost events on a crash after
	// commit, and (because it ran on the request context) most events
	// outright.

	// ── Transactional outbox (ORG §9.2) ──────────────────────────────────────
	//
	// The ORG-02/ORG-03 guarded writes enqueue their events into event_outbox
	// inside the business transaction; this relay drains that table to Kafka.
	// The pre-existing write paths still publish directly through
	// eventPublisher, so both mechanisms are live at once and deliver the same
	// envelope to the same topic — a consumer cannot tell them apart.
	//
	// An unreachable broker is not fatal. Events accumulate in Postgres and are
	// delivered when it returns; a registry that refuses to start because Kafka
	// is down would be a worse outage than the one it is reacting to.
	outboxStore := outbox.NewStore(pool, log)
	pgStore.SetOutbox(outboxStore)

	relay := outbox.NewRelay(pool, kafkaWriterAdapter{kafkaWriter}, outbox.DefaultRelayConfig(), log)
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go relay.Run(relayCtx)

	// Authorization client. Refuses to start in production or staging against
	// a placeholder URL, rather than silently falling back to a permit-all
	// stub. The previous form treated any URL other than the literal
	// "http://authorization-svc" as production wiring — and docker-compose
	// set it to this service's own address, so it selected a client whose
	// Authorize was a TODO returning nil and logged "using HTTP authorization
	// client" while permitting every mutation without a decision.
	authzClient, err := authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
	if err != nil {
		log.Fatal("authz client construction failed", zap.Error(err))
	}

	// Jurisdiction validator. The stub accepts every id; config.validate
	// refuses to start staging or production with it (it used to be selected
	// silently in any environment whose URL was unset or left at default).
	var jurisdValidator jurisdiction.JurisdictionValidator = jurisdiction.NewStubValidator(log)
	if cfg.JurisdictionValidatorIsReal() {
		jurisdValidator = jurisdiction.NewHTTPValidator(cfg.JurisdictionRulesURL, log)
		log.Info("using HTTP jurisdiction validator", zap.String("url", cfg.JurisdictionRulesURL))
	} else {
		log.Warn("using STUB jurisdiction validator — wire real service before production")
	}

	// ── 5. Service ───────────────────────────────────────────────────────────
	svc := registry.NewService(pgStore, authzClient, jurisdValidator, cfg.AuthZPlatformScopeID, log)
	svc.ConfigureMakerChecker(cfg.MakerCheckerLegacyBodyApprover, time.Duration(cfg.ApprovalTTLHours)*time.Hour)
	svc.ConfigureCompatibility(cfg.LegacyEntityCreateActive, cfg.OnboardingKeyOptional)

	// ORG-02 §4.2 server-resolved provisioning context: plan entitlement from
	// commercial-account-svc and the restricted-jurisdiction list.
	var entitlementChecker entitlement.Checker = entitlement.NewStubChecker(log)
	if cfg.CommercialAccountURL != "" {
		entitlementChecker = entitlement.NewHTTPChecker(cfg.CommercialAccountURL, log)
		log.Info("using HTTP entitlement checker", zap.String("url", cfg.CommercialAccountURL))
	} else {
		log.Warn("using STUB entitlement checker — local only; refused in staging/production")
	}
	svc.ConfigureProvisioning(entitlementChecker, cfg.RestrictedJurisdictionCodes, cfg.LegacyProvisioningInputs)
	svc.ConfigureConcurrency(cfg.ExpectedVersionOptional)

	// ── 6. HTTP router ───────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("tenant-entity-registry-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	// Extract the caller's gateway-verified identity into the request context:
	// tenant_id so every DB call can set app.tenant_id on the Postgres session
	// and RLS is actually enforced, and principal_id so mutations are
	// authorized and audited against a real caller. Previously both were
	// base64-decoded out of an unverified JWT.
	r.Use(svcmiddleware.Identity(log))
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(svc, log)
	// Idempotency-Key replay protection (migration 000010) — ORG shared
	// contract §3, §9.2 DoD gates 2 and 4.
	handler.RegisterRoutes(r, h, idempotency.Middleware(pgStore, log))
	go purgeIdempotencyKeys(relayCtx, pgStore, log)
	go watchOutbox(relayCtx, relay, metrics, log)

	// ── 7. Health probes (separate path, no auth) ────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

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
	// Stop the relay after the HTTP server, not before: a request in flight
	// during shutdown can still enqueue an event, and draining first would
	// leave it for the next process start rather than delivering it now.
	stopRelay()
	log.Info("server stopped")
}

// kafkaWriterAdapter bridges outbox.KafkaMessage to kafka.Message.
//
// The outbox package declares its own two-field message type rather than
// importing kafka-go, so a test fake for the relay does not drag the broker
// client in with it. This adapter is the one place the two meet.
type kafkaWriterAdapter struct{ w *kafka.Writer }

func (a kafkaWriterAdapter) WriteMessages(ctx context.Context, msgs ...outbox.KafkaMessage) error {
	out := make([]kafka.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, kafka.Message{Key: m.Key, Value: m.Value})
	}
	return a.w.WriteMessages(ctx, out...)
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

// idempotencyRetention is how long a recorded command response answers a
// retry. A week covers any realistic client retry window.
const idempotencyRetention = 7 * 24 * time.Hour

// purgeIdempotencyKeys drops expired replay records hourly, so the table
// migration 000010 added does not grow without bound. Stops with the relay.
func purgeIdempotencyKeys(ctx context.Context, s *store.PgStore, log *zap.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := s.PurgeIdempotencyKeysBefore(ctx, time.Now().UTC().Add(-idempotencyRetention))
		if err != nil && ctx.Err() == nil {
			log.Error("idempotency key purge failed", zap.Error(err))
		} else if n > 0 {
			log.Info("idempotency keys purged", zap.Int64("rows", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchOutbox exports event-delivery gauges every 15s (SLO.md "event
// delivery"). The relay already knew these numbers; nothing exported them.
func watchOutbox(ctx context.Context, relay *outbox.Relay, m *telemetry.Metrics, log *zap.Logger) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if n, err := relay.PendingCount(ctx); err == nil {
			m.OutboxPending.Set(float64(n))
		} else if ctx.Err() == nil {
			log.Warn("outbox pending count failed", zap.Error(err))
		}
		if n, err := relay.DeadLetterCount(ctx); err == nil {
			m.OutboxDeadLetter.Set(float64(n))
		}
		published, failed := relay.Stats()
		m.OutboxPublished.Set(float64(published))
		m.OutboxFailed.Set(float64(failed))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
