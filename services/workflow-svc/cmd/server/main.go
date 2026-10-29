// Package main is the entry point for workflow-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore, Kafka producer, authorization-svc client
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/workflow-svc/internal/authz"
	"zoiko.io/workflow-svc/internal/config"
	"zoiko.io/workflow-svc/internal/consumer"
	"zoiko.io/workflow-svc/internal/decisionlog"
	"zoiko.io/workflow-svc/internal/documentvault"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	"zoiko.io/workflow-svc/internal/events"
	"zoiko.io/workflow-svc/internal/evidence"
	"zoiko.io/workflow-svc/internal/handler"
	"zoiko.io/workflow-svc/internal/health"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/outbox"
	"zoiko.io/workflow-svc/internal/registry"
	"zoiko.io/workflow-svc/internal/store"
	"zoiko.io/workflow-svc/internal/telemetry"
	"zoiko.io/workflow-svc/internal/timers"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	// The governance ledger relay cannot authenticate without its service
	// principal; retrying an unauthenticated delivery forever hides a
	// configuration error. Local runs may omit it when no ledger is wired.
	if cfg.Env != "local" && cfg.LedgerPrincipalID == "" {
		log.Fatal("WORKFLOW_LEDGER_PRINCIPAL_ID is required outside local: decision ledger records need a service principal")
	}

	log.Info("workflow-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authorization_service_url", cfg.AuthorizationServiceURL),
		zap.String("document_vault_service_url", cfg.DocumentVaultServiceURL),
	)

	shutdownTracing, err := telemetry.InitTracing(context.Background(), "workflow-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("workflow-svc")

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
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	pgStore := store.New(pool, log)

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

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	authzClient := authz.NewHTTPClient(cfg.AuthorizationServiceURL, log)
	documentVaultClient := documentvault.NewHTTPClient(cfg.DocumentVaultServiceURL, log)

	// Identity gate (X5 / X-R3): a signed envelope, when the caller sends one,
	// is verified against identity-context-svc's JWKS and its session status,
	// and its claims override X-Principal-Id / X-Tenant-Id. Without this the
	// gateway's raw headers were the only identity, and this service could not
	// pass a verified envelope on to authorization-svc under
	// AUTHZ_IDENTITY_ASSERTION=require (the cross-domain auth plane).
	gate, gateSettings, err := idenv.NewGateFromEnv("workflow-svc", log, idenv.EnvOptions{})
	if err != nil {
		log.Fatal("identity gate configuration refused", zap.Error(err))
	}
	log.Info("identity gate", gateSettings.Fields()...)

	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(otelchi.Middleware("workflow-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(chimiddleware.Logger)
	r.Use(gate.Middleware)
	r.Use(svcmiddleware.TenantContext())

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	// Refusals rendered as RFC 9457 problem+json, like every other error here.
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, publisher, authzClient, documentVaultClient, log)
	h = h.WithRegistryChecker(registry.New(cfg.RegistryServiceURL, log))
	h = h.WithReadAuthz(cfg.ReadAuthzMode)
	// AUD-07's contradiction check reads document-vault-svc's own AUD-06
	// evidence state; without this wiring the check was skipped silently and
	// a contradictory workpaper could be marked prepared.
	h = h.WithEvidenceClient(evidence.NewHTTPClient(cfg.DocumentVaultServiceURL, log))
	handler.RegisterRoutes(r, h)

	// Outbox relays (ZS-STATE-001 Invariant I-13 / doc7 item 32):
	// one Relay per destination — Kafka for domain events, decision_log for
	// the governance ledger record written in the decision's own transaction.
	// Each polls with FOR UPDATE SKIP LOCKED, so replicas never double-send.
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	kafkaRelay := outbox.NewRelay(pool, publisher.Deliverer(), outbox.DestinationKafka, 1500*time.Millisecond, 50, log)
	go kafkaRelay.Start(relayCtx)
	ledgerRelay := outbox.NewRelay(pool,
		decisionlog.NewLedgerDeliverer(cfg.GovernanceDecisionLogServiceURL, cfg.LedgerPrincipalID),
		outbox.DestinationDecisionLog, 1500*time.Millisecond, 50, log)
	go ledgerRelay.Start(relayCtx)

	// Deadline expiry, escalation and authority re-check (000020).
	sweeper := timers.New(pgStore, authzClient, cfg.TimerSweepInterval, cfg.TimerSweepBatch, log, prometheus.DefaultRegisterer)
	go sweeper.Start(relayCtx)

	// Authority-change consumer: a policy or delegation/authority change, or
	// an authorization denial, flags the affected pending approvals so the
	// sweeper re-authorizes their approvers.
	recheckConsumer := consumer.New(cfg.Kafka.Brokers, cfg.ConsumerGroupID, cfg.ConsumerTopics, pgStore, log)
	recheckConsumer.Start(relayCtx)
	defer recheckConsumer.Close()

	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	addr := ":" + strconv.Itoa(cfg.Port)
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

	relayCancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("server stopped")
}

// correlationIDMiddleware echoes the caller's X-Correlation-ID. It used to
// invent one from the request id when the caller sent none, before the input
// contract below could refuse the request: correlation_id is mandatory
// end-to-end (§4), and a made-up one traces nothing (Governance audit row 34).
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}
