package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/capability-registry-svc/internal/authz"
	"zoiko.io/capability-registry-svc/internal/config"
	"zoiko.io/capability-registry-svc/internal/domain"
	svcenvelope "zoiko.io/capability-registry-svc/internal/envelope"
	"zoiko.io/capability-registry-svc/internal/handler"
	"zoiko.io/capability-registry-svc/internal/health"
	"zoiko.io/capability-registry-svc/internal/middleware"
	"zoiko.io/capability-registry-svc/internal/mtls"
	"zoiko.io/capability-registry-svc/internal/store"
	"zoiko.io/capability-registry-svc/internal/telemetry"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = domain.PlatformScopeID

func main() {
	logger, err := telemetry.NewLogger("capability-registry-svc")
	if err != nil {
		fmt.Printf("failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		logger.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	// pgxpool connects lazily, so an error here is a bad config, not an
	// unreachable database. The old code carried on with a nil pool; the
	// schema check below needs a pool, and a service with no pool cannot
	// serve a single request, so this is fatal now.
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Fatal("failed to create database pool", zap.Error(err))
	}

	// The eventing outbox table must exist before the first business write
	// tries to enqueue into it; fail here, at deploy time, not on a request.
	if err := outbox.VerifySchema(ctx, pool); err != nil {
		logger.Fatal("eventing outbox schema missing", zap.Error(err))
	}
	logger.Info("connected to postgres database")

	pgStore := store.NewPgStore(pool, store.WithEventRegion(cfg.EventResidencyRegion))

	// Transactional outbox relay (ZS-EVENT-001 §6). Every write enqueues its
	// event into eventing_outbox in the same transaction as the state change;
	// this relay delivers them with leased claims (safe across replicas),
	// capped backoff with jitter, quarantine and a broker-health probe.
	// kafkaout's writer acknowledges with RequireAll — a fire-and-forget
	// writer must never back the outbox, or a "published" event could be one
	// no broker actually stored.
	outboxWriter, err := kafkaout.New(kafkaout.Config{
		Brokers: strings.Split(cfg.KafkaBrokers, ","),
		Topic:   cfg.KafkaEventsTopic,
	})
	if err != nil {
		logger.Fatal("outbox kafka writer", zap.Error(err))
	}
	defer func() { _ = outboxWriter.Close() }()
	relay, err := outbox.NewRelay(pool, outboxWriter, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("outbox relay", zap.Error(err))
	}
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "capability-registry-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	h := handler.New(pgStore, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext())

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting capability-registry-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down capability-registry-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}
	// Stop the relay after the server: requests finishing during Shutdown
	// may still commit events. Anything not delivered stays in the outbox
	// for the next process — stopping loses nothing.
	stopRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		logger.Warn("outbox relay did not stop before the shutdown deadline")
	}
	pool.Close()
	logger.Info("server stopped cleanly")
}
