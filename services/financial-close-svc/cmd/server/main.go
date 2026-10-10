package main

import (
	"context"
	"errors"
	"math"
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"go.uber.org/zap"

	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/config"
	"zoiko.io/financial-close-svc/internal/consumer"
	svcenvelope "zoiko.io/financial-close-svc/internal/envelope"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/health"
	svckafka "zoiko.io/financial-close-svc/internal/kafka"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/mtls"
	"zoiko.io/financial-close-svc/internal/store"
	"zoiko.io/financial-close-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented to mtls-management-svc
// when provisioning this service's own client-side mTLS identity — the same
// platform-wide scope tax-rules-svc's mTLS pilot uses.
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

	log.Info("financial-close-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.String("ledger_url", cfg.LedgerServiceURL),
		zap.String("ap_url", cfg.APServiceURL),
		zap.String("ar_url", cfg.ARServiceURL),
		zap.String("vault_url", cfg.VaultServiceURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "financial-close-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("financial-close-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
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

	// Verify connectivity at startup
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// The eventing outbox table must exist before the first business write
	// tries to enqueue into it; fail here, at deploy time, not on a request.
	if err := outbox.VerifySchema(pingCtx, pool); err != nil {
		log.Fatal("eventing outbox schema missing", zap.Error(err))
	}

	// ── 4. Store, Kafka producer, clients ─────────────────────────────────────
	pgStore := store.New(pool, log, store.WithEventRegion(cfg.EventResidencyRegion))

	// ── 4b. Transactional outbox relay (ZS-EVENT-001 §6) ─────────────────────
	// Period lifecycle events are written to eventing_outbox in the same
	// transaction as the period state change; this relay delivers them.
	// kafkaout builds its own writer because delivery must be acknowledged
	// (RequireAll) — a fire-and-forget writer must never back the outbox,
	// or a "published" event could be one no broker stored.
	outboxWriter, err := kafkaout.New(kafkaout.Config{
		Brokers:                cfg.Kafka.Brokers,
		Topic:                  cfg.Kafka.Topic,
		AllowAutoTopicCreation: cfg.Env == "local",
	})
	if err != nil {
		log.Fatal("outbox kafka writer", zap.Error(err))
	}
	defer func() { _ = outboxWriter.Close() }()
	relay, err := outbox.NewRelay(pool, outboxWriter, outbox.DefaultConfig(), log)
	if err != nil {
		log.Fatal("outbox relay", zap.Error(err))
	}
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	// Buffered send rather than close(): this file imports a package named
	// close, which shadows the builtin.
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()
	registerOutboxMetrics(relay, log)

	var clientsWrapper *clients.Clients
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "financial-close-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		clientsWrapper = clients.NewWithAuthzHTTPClient(cfg.AuthzMTLSURL, cfg.LedgerServiceURL, cfg.APServiceURL, cfg.ARServiceURL, cfg.VaultServiceURL, cfg.AssetServiceURL, cfg.InventoryServiceURL, cfg.ProjectServiceURL, log, mtlsHTTPClient)
	} else {
		clientsWrapper = clients.New(cfg.AuthZServiceURL, cfg.LedgerServiceURL, cfg.APServiceURL, cfg.ARServiceURL, cfg.VaultServiceURL, cfg.AssetServiceURL, cfg.InventoryServiceURL, cfg.ProjectServiceURL, log)
	}

	clientsWrapper.WithFinancialControlURL(cfg.FinancialControlServiceURL)
	clientsWrapper.WithBankingURLs(cfg.TreasuryServiceURL, cfg.BankReconciliationServiceURL)
	if cfg.CloseGateModeInvalid {
		log.Warn("unrecognised FINCTRL_CLOSE_GATE_MODE; treating as enforce (fail closed)")
	}
	log.Info("financial-control close gate", zap.String("mode", cfg.CloseGateMode))
	if cfg.SubledgerControlGateModeInvalid {
		log.Warn("unrecognised SUBLEDGER_CONTROL_GATE_MODE; treating as enforce (fail closed)")
	}
	if cfg.BankReconGateModeInvalid {
		log.Warn("unrecognised BANK_RECON_GATE_MODE; treating as enforce (fail closed)")
	}
	if cfg.BankReconGateMode == "off" {
		log.Warn("bank reconciliation close gate is OFF: periods can close without reconciled cash. Use only where no bank feed exists yet.")
	} else {
		log.Info("bank reconciliation close gate", zap.String("mode", cfg.BankReconGateMode), zap.Int("cutoff_days", cfg.BankReconCutoffDays))
	}
	if cfg.SubledgerControlGateMode == "off" {
		log.Warn("subledger control close gate is OFF: periods can close without proving AR/AP agree with the GL (ACC-06). Use only until control-account mappings are configured.")
	} else {
		log.Info("subledger control close gate", zap.String("mode", cfg.SubledgerControlGateMode))
	}

	// ── 4c. Lineage Kafka consumer ─────────────────────────────────────────────
	// Consumes asset-management-svc's, inventory-management-svc's and
	// project-accounting-svc's own "accounting event emitted" signals to
	// build ACC-18 lineage edges — the AST/INV/PRJ domain spec's own §9
	// "source-to-report" assertion. Event-driven rather than an inbound
	// HTTP push from those three services: Kafka's own durable,
	// at-least-once delivery means an outage here delays lineage
	// recording, never loses it. Started before the HTTP listener, same
	// reasoning as identity-context-svc's own revocation consumer — an
	// event published while this service was down must still be
	// consumed once it's back, not skipped.
	lineageConsumer := consumer.New(pgStore, log)
	consumerCtx, stopConsumers := context.WithCancel(context.Background())
	defer stopConsumers()
	lineageRunners := []*svckafka.Runner{
		svckafka.NewRunner(cfg.Kafka.Brokers, cfg.Kafka.GroupID, cfg.AssetEventsTopic, lineageConsumer, metrics, log),
		svckafka.NewRunner(cfg.Kafka.Brokers, cfg.Kafka.GroupID, cfg.InventoryEventsTopic, lineageConsumer, metrics, log),
		svckafka.NewRunner(cfg.Kafka.Brokers, cfg.Kafka.GroupID, cfg.ProjectEventsTopic, lineageConsumer, metrics, log),
	}
	for _, run := range lineageRunners {
		go run.Run(consumerCtx)
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("financial-close-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, clientsWrapper, clientsWrapper, []byte(cfg.CloseSigningKey), log).SetCloseGateEnforced(cfg.CloseGateMode == "enforce").
		SetSubledgerControlGateEnforced(cfg.SubledgerControlGateMode == "enforce").
		SetBankReconciliationGate(cfg.BankReconGateMode == "enforce", cfg.BankReconCutoffDays)
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

	// Stop the relay after the server: requests finishing during Shutdown
	// may still commit events. Anything not delivered stays in the outbox
	// for the next process — stopping loses nothing.
	cancelRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		log.Warn("outbox relay did not stop before the shutdown deadline")
	}

	log.Info("stopping lineage kafka consumers")
	stopConsumers()
	// Bounded — a stuck reader/commit must not block process exit
	// forever. Close() is called even on timeout so the underlying
	// connections are released either way.
	drained := make(chan struct{})
	go func() {
		for _, run := range lineageRunners {
			run.Close()
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		log.Error("lineage consumer shutdown timed out")
	}

	log.Info("server stopped")
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}

// registerOutboxMetrics exposes the outbox backlog signals ZS-EVENT-001 §6.1
// calls first-class. Read at scrape time; each read is three indexed counts.
// Alert on outbox_oldest_backlog_age_seconds that only grows (relay stopped or
// broker gone while the service otherwise looks healthy) and on any
// quarantined event (needs an owner's decision).
func registerOutboxMetrics(relay *outbox.Relay, log *zap.Logger) {
	read := func(pick func(outbox.Stats) float64) func() float64 {
		return func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			st, err := relay.ReadStats(ctx)
			if err != nil {
				log.Warn("outbox stats unavailable", zap.Error(err))
				return math.NaN()
			}
			return pick(st)
		}
	}
	labels := prometheus.Labels{"service": "financial-close-svc"}
	prometheus.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "outbox_backlog_events", Help: "Events committed but not yet published.", ConstLabels: labels,
		}, read(func(s outbox.Stats) float64 { return float64(s.Backlog) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "outbox_quarantined_events", Help: "Events quarantined after exhausting retries or a permanent broker rejection.", ConstLabels: labels,
		}, read(func(s outbox.Stats) float64 { return float64(s.Quarantined) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "outbox_oldest_backlog_age_seconds", Help: "Age of the oldest undelivered event.", ConstLabels: labels,
		}, read(func(s outbox.Stats) float64 { return s.OldestBacklogAge.Seconds() })),
	)
}