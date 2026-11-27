// Package main is the entry point for general-ledger-svc.
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
	"fmt"
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
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/general-ledger-svc/internal/authz"
	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/config"
	"zoiko.io/general-ledger-svc/internal/currency"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	"zoiko.io/general-ledger-svc/internal/events"
	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/certwatch"
	"zoiko.io/general-ledger-svc/internal/evidencereq"
	"zoiko.io/general-ledger-svc/internal/handler"
	"zoiko.io/general-ledger-svc/internal/health"
	"zoiko.io/general-ledger-svc/internal/kernel"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/mtls"
	"zoiko.io/general-ledger-svc/internal/periodgate"
	"zoiko.io/general-ledger-svc/internal/store"
	"zoiko.io/general-ledger-svc/internal/telemetry"
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

	log.Info("general-ledger-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "general-ledger-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("general-ledger-svc")

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

	// The eventing outbox table must exist before the first journal write
	// tries to enqueue into it; fail here, at deploy time, not on a request.
	if err := outbox.VerifySchema(pingCtx, pool); err != nil {
		log.Fatal("eventing outbox schema missing", zap.Error(err))
	}

	// ── 4. Store, Kafka producer, jurisdiction validator ─────────────────────
	pgStore := store.New(pool, log, store.WithEventRegion(cfg.EventResidencyRegion))

	// Kafka producer — connects lazily on first write, same posture as
	// identity-context-svc/tenant-entity-registry-svc/policy-svc: not a
	// fail-fast startup dependency like Postgres.
	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	// BatchTimeout is 10ms, not the library default of 1s. Publishing here is
	// synchronous on the request path — every journal create, validate, post
	// and reverse waits for the write — so the default made each of those
	// calls take about a second of pure batching delay. Same value, same
	// reason, as every other producer in this estate.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)

	// ── 4b. Transactional outbox relay (ZS-EVENT-001 §6) ─────────────────────
	// Journal events are written to eventing_outbox in the same transaction
	// as the journal change; this relay delivers them. kafkaout builds its
	// own writer because delivery must be acknowledged (RequireAll) — the
	// writer above is kafka-go's fire-and-forget default and must never back
	// the outbox, or a "published" event could be one no broker stored.
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

	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "general-ledger-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, log, mtlsHTTPClient)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}

	legacyClose := close.NewHTTPClient(cfg.CloseServiceURL, log)
	// REF-05 shadow gate (phase 2): off => closeClient IS legacyClose (no gate
	// client, no outbound request, no metrics); shadow => compare-only wrapper
	// that always returns the legacy result. "enforce" never reaches here:
	// config.Load rejects it.
	closeClient := periodgate.Wrap(periodgate.Settings{
		Mode: cfg.PeriodGateMode, URL: cfg.AccountingPeriodURL, Timeout: cfg.PeriodGateShadowTimeout,
	}, legacyClose, prometheus.DefaultRegisterer, log)
	log.Info("period gate configured", zap.String("mode", cfg.PeriodGateMode))

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("general-ledger-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	// Reads the caller's tenant scope from X-Tenant-Id (set by
	// gateway-auth-svc's ForwardAuth verification) into context, so every DB
	// call can filter by it explicitly — see internal/store's doc comment
	// on why RLS alone is not sufficient here.
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	manualPolicy, err := handler.ParseManualJournalPolicy(cfg.ManualJournalEvidenceThreshold, cfg.ManualJournalEvidenceTypes)
	if err != nil {
		log.Fatal("manual journal policy", zap.Error(err))
	}
	// The certification watch (ZS-SVC-Z-001 RB-05): recording a journal's or posting's reliance also puts the certification on the
	// watch list, and a sweep flags what rests on one that was revoked.
	certClient := certreliance.NewClient(cfg.DataQualityServiceURL, cfg.ReconciliationServiceURL)
	certWatchRepo := store.NewCertWatchRepo(pool)
	certRecorder := &certwatch.Recorder{Reliance: certClient, Store: certWatchRepo}
	certSweeper := &certwatch.Sweeper{Store: certWatchRepo, Status: certClient}
	// The watch's metrics: the schedule, the flags and the event consumer. The open-flag gauges are read at scrape time, as the outbox
	// backlog is, and report NaN (never a reassuring zero) when the read fails.
	certMetrics := telemetry.NewCertWatchMetrics("general-ledger-svc")
	telemetry.RegisterFlagGauges("general-ledger-svc", certWatchRepo.OpenFlagStats, func(err error) { log.Warn("certification flag stats unavailable", zap.Error(err)) })
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	// The scheduled sweep (certwatch.Scheduler): it starts only when CERT_SWEEP_PRINCIPAL_ID names the principal it acts as, and says so
	// in the log when it does not. It acts as that principal, authorized per legal entity like any caller.
	certScheduler := &certwatch.Scheduler{Sweeper: certSweeper, Scopes: certWatchRepo, Authz: authzClient, Lock: certWatchRepo,
		Principal: cfg.CertSweepPrincipalID, Interval: cfg.CertSweepInterval, ScopeLimit: 2 * time.Minute, Log: log, Observer: certMetrics}
	go certScheduler.Run(sweepCtx)
	// The event-driven trigger: hears about a revocation from the issuing service's reliance-withdrawn event in seconds, instead of at the
	// next sweep. The event is only a trigger (the issuing service is asked what state the certification is in), so a lost, duplicated or
	// forged event cannot raise a false flag, and the schedule above is the backstop. It acts as the same principal.
	if cfg.CertSweepPrincipalID != "" && cfg.CertWithdrawalConsumer {
		reactor := &certwatch.Reactor{Store: certWatchRepo, Sweeper: certSweeper, Authz: authzClient, Principal: cfg.CertSweepPrincipalID, Log: log, Observer: certMetrics}
		withdrawalReader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: cfg.Kafka.Brokers, GroupTopics: cfg.CertWithdrawalTopics, GroupID: cfg.CertWithdrawalGroupID,
			ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
				log.Debug("certification withdrawal consumer kafka reader: " + fmt.Sprintf(msg, args...))
			}),
		})
		go (&certwatch.Consumer{Reactor: reactor, Log: log, Observer: certMetrics}).Run(sweepCtx, withdrawalReader)
	} else {
		log.Info("certification withdrawal consumer not started", zap.Bool("principal_configured", cfg.CertSweepPrincipalID != ""), zap.Bool("enabled", cfg.CertWithdrawalConsumer))
	}
	h := handler.New(pgStore, publisher, authzClient, closeClient, log).
		WithEvidenceGate(evidencereq.NewClient(cfg.EvidenceRequirementsURL).WithLogger(log)).
		WithCertificationReliance(certRecorder).
		WithCertificationWatch(certSweeper, certWatchRepo).
		WithCertificationObserver(certMetrics).
		WithJournalControls(pgStore, manualPolicy)
	handler.RegisterRoutes(r, h)

	// Global Accounting Kernel (Wave 0): books, accounting events, posting rule sets, dry-run evaluation, dimensions.
	kernelRepo := store.NewKernelRepo(pool, cfg.EventResidencyRegion)
	kernelSvc := &kernel.Service{Repo: kernelRepo, Reliance: certRecorder}
	kernelH := handler.NewKernel(kernelSvc, kernelRepo, kernelRepo, currency.New(cfg.CurrencyRegistryURL), nil, authzClient, log).
		WithPeriodGate(handler.ClosePeriodGate{Close: closeClient, Authz: authzClient}).
		WithObserver(telemetry.NewKernelMetrics("general-ledger-svc"))
	if cfg.SnapshotSigningKey != "" {
		if len(cfg.SnapshotSigningKey) < 32 {
			log.Fatal("SNAPSHOT_SIGNING_KEY must be at least 32 characters")
		}
		kernelH.WithSigner(kernel.HMACSigner{Key: []byte(cfg.SnapshotSigningKey), KeyID: cfg.SnapshotSigningKeyID})
	} else {
		log.Warn("SNAPSHOT_SIGNING_KEY is not set: balance snapshots cannot be signed until it is")
	}
	handler.RegisterKernelRoutes(r, kernelH)

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
	labels := prometheus.Labels{"service": "general-ledger-svc"}
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
