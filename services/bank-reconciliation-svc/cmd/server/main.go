// Package main is the entry point for bank-reconciliation-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore, Kafka producer, authorization-svc + general-ledger-svc clients
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

	"zoiko.io/bank-reconciliation-svc/internal/authz"
	"zoiko.io/bank-reconciliation-svc/internal/banking"
	"zoiko.io/bank-reconciliation-svc/internal/close"
	"zoiko.io/bank-reconciliation-svc/internal/config"
	svcenvelope "zoiko.io/bank-reconciliation-svc/internal/envelope"
	"zoiko.io/bank-reconciliation-svc/internal/events"
	"zoiko.io/bank-reconciliation-svc/internal/handler"
	"zoiko.io/bank-reconciliation-svc/internal/health"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
	"zoiko.io/bank-reconciliation-svc/internal/mtls"
	"zoiko.io/bank-reconciliation-svc/internal/store"
	"zoiko.io/bank-reconciliation-svc/internal/telemetry"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
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

	log.Info("bank-reconciliation-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.String("ledger_url", cfg.LedgerServiceURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "bank-reconciliation-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("bank-reconciliation-svc")

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

	// The eventing outbox table must exist before the first reconciliation
	// write tries to enqueue into it; fail here, at deploy time, not on a
	// request.
	if err := outbox.VerifySchema(pingCtx, pool); err != nil {
		log.Fatal("eventing outbox schema missing", zap.Error(err))
	}

	// ── 4. Store, outbox relay, jurisdiction validator ───────────────────────
	pgStore := store.New(pool, log, store.WithEventRegion(cfg.EventResidencyRegion))

	// ── 4b. Transactional outbox relay (ZS-EVENT-001 §6) ─────────────────────
	// Reconciliation lifecycle events are written to eventing_outbox in the
	// same transaction as the state change; this relay delivers them. This
	// replaces internal/events.Publisher's direct, post-commit Kafka write,
	// which could silently lose an event on a broker outage or a crash
	// between the commit and the publish call. kafkaout builds its own
	// writer because delivery must be acknowledged (RequireAll) — a
	// fire-and-forget writer must never back the outbox, or a "published"
	// event could be one no broker actually stored.
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
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()
	registerOutboxMetrics(relay, log)

	// Evidence-conflicts consumer: subscribes to payment-status-svc's own
	// events topic (not ours) for PAYMENT_STATUS_CONFLICT_RAISED, and
	// turns it into a real evidence_conflicts row via ConflictConsumer.
	// Before this, POST /v1/evidence-conflicts existed but nothing ever
	// called it automatically — payment-status-svc's published event had
	// no subscriber. Same non-fatal-broker posture as every other
	// consumer in this platform: reconciliation itself does not depend on
	// this feed.
	conflictConsumerCtx, stopConflictConsumer := context.WithCancel(context.Background())
	defer stopConflictConsumer()
	conflictReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: cfg.Kafka.Brokers,
		Topic:   cfg.Kafka.PaymentStatusEventsTopic,
		GroupID: cfg.Kafka.ConflictConsumerGroupID,
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
			log.Debug("evidence-conflict consumer kafka reader: " + fmt.Sprintf(msg, args...))
		}),
	})
	conflictConsumer := events.NewConflictConsumer(log, pgStore)
	go conflictConsumer.Run(conflictConsumerCtx, conflictReader)

	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "bank-reconciliation-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewHTTPClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, log)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}

	ledgerClient := ledger.NewHTTPClient(cfg.LedgerServiceURL)

	bankingClient := banking.NewHTTPClient(cfg.BankingConnectorURL)
	closeClient := close.NewHTTPClient(cfg.CloseServiceURL, log)

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("bank-reconciliation-svc", otelchi.WithChiRoutes(r)))
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

	h := handler.New(pgStore, authzClient, ledgerClient, bankingClient, closeClient, log)
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
	log.Info("server stopped")
}

// registerOutboxMetrics exposes the outbox backlog signals ZS-EVENT-001 §6.1
// calls first-class. Read at scrape time; each read is three indexed counts.
// Alert on outbox_oldest_backlog_age_seconds that only grows (relay stopped
// or broker gone while the service otherwise looks healthy) and on any
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
	labels := prometheus.Labels{"service": "bank-reconciliation-svc"}
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
