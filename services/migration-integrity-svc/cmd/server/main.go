package main

import (
	"context"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/config"
	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/mtls"
	"zoiko.io/migration-integrity-svc/internal/store"
	"zoiko.io/migration-integrity-svc/internal/telemetry"
)

// platformScopeID is the platform-wide legal-entity scope used when this
// service provisions its own mTLS client identity from mtls-management-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting migration-integrity-svc", zap.String("port", cfg.Port))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Fatal("failed to create db pool", zap.Error(err))
	}
	defer pool.Close()
	// No in-memory fallback: this service records migration certification
	// evidence, and both that evidence and its outbox events live in Postgres.
	// Serving from memory would accept evidence that vanishes on restart and
	// emits no event at all.
	if err := pool.Ping(ctx); err != nil {
		logger.Fatal("db unreachable at startup", zap.Error(err))
	}
	logger.Info("Connected to PostgreSQL")

	// The eventing outbox table must exist before the first business write
	// tries to enqueue into it; fail here, at deploy time, not on a request.
	if err := outbox.VerifySchema(ctx, pool); err != nil {
		logger.Fatal("eventing outbox schema missing", zap.Error(err))
	}

	dataStore := store.NewPgStore(pool, logger, store.WithEventRegion(cfg.EventResidencyRegion))

	// ── Transactional outbox relay (ZS-EVENT-001 §6) ─────────────────────────
	// Evidence events are written to eventing_outbox in the same transaction
	// as the state change; this relay delivers them. kafkaout builds its own
	// writer because delivery must be acknowledged (RequireAll) — a
	// fire-and-forget writer must never back the outbox.
	outboxWriter, err := kafkaout.New(kafkaout.Config{
		Brokers:                strings.Split(cfg.KafkaBrokers, ","),
		Topic:                  cfg.KafkaTopic,
		AllowAutoTopicCreation: cfg.Env == "local",
	})
	if err != nil {
		logger.Fatal("outbox kafka writer", zap.Error(err))
	}
	defer func() { _ = outboxWriter.Close() }()
	relay, err := outbox.NewRelay(pool, outboxWriter, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("outbox relay", zap.Error(err))
	}
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()
	registerOutboxMetrics(relay, logger)

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "migration-integrity-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}
	h := handler.NewHandler(dataStore, authzClient, logger)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", handler.NewRouter(h))

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	logger.Info("Server listening on :" + cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)

	// Stop the relay after the server: requests finishing during Shutdown
	// may still commit events. Anything not delivered stays in the outbox
	// for the next process — stopping loses nothing.
	cancelRelay()
	select {
	case <-relayDone:
	case <-shutCtx.Done():
		logger.Warn("outbox relay did not stop before the shutdown deadline")
	}
	logger.Info("Server exited")
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
	labels := prometheus.Labels{"service": "migration-integrity-svc"}
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
