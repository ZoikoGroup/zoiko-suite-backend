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

	"zoiko.io/comments-collaboration-svc/internal/authz"
	"zoiko.io/comments-collaboration-svc/internal/config"
	svcenvelope "zoiko.io/comments-collaboration-svc/internal/envelope"
	"zoiko.io/eventing/kafkaout"
	eventingoutbox "zoiko.io/eventing/outbox"
	"zoiko.io/comments-collaboration-svc/internal/handler"
	"zoiko.io/comments-collaboration-svc/internal/health"
	"zoiko.io/comments-collaboration-svc/internal/middleware"
	"zoiko.io/comments-collaboration-svc/internal/mtls"
	"zoiko.io/comments-collaboration-svc/internal/retention"
	"zoiko.io/comments-collaboration-svc/internal/store"
	"zoiko.io/comments-collaboration-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	logger, err := telemetry.NewLogger("comments-collaboration-svc")
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
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Warn("unable to connect to database on startup", zap.Error(err))
	} else {
		logger.Info("connected to postgres database")
	}

	// The shared eventing outbox table must exist before the first write tries to enqueue into it.
	if pool != nil {
		if err := eventingoutbox.VerifySchema(ctx, pool); err != nil {
			logger.Fatal("eventing outbox schema missing", zap.Error(err))
		}
	}
	pgStore := store.NewPgStore(pool, store.WithEventRegion(cfg.EventResidencyRegion))
	brokers := strings.Split(cfg.KafkaBrokers, ",")

	// Relay for the BIZ-09 events on the shared eventing outbox. With no database or no brokers the
	// relay is not started: events queue in eventing_outbox and none are lost.
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	eventingRelayDone := make(chan struct{}, 1)
	if pool == nil || cfg.KafkaBrokers == "" {
		logger.Warn("no database or KAFKA_BROKERS: the eventing outbox relay is not running; BIZ-09 events will queue in eventing_outbox")
		eventingRelayDone <- struct{}{}
	} else {
		eventingWriter, err := kafkaout.New(kafkaout.Config{
			Brokers:                brokers,
			Topic:                  cfg.KafkaEventsTopic,
			AllowAutoTopicCreation: cfg.Env == "local",
		})
		if err != nil {
			logger.Fatal("eventing outbox kafka writer", zap.Error(err))
		}
		defer func() { _ = eventingWriter.Close() }()
		eventingRelay, err := eventingoutbox.NewRelay(pool, eventingWriter, eventingoutbox.DefaultConfig(), logger)
		if err != nil {
			logger.Fatal("eventing outbox relay", zap.Error(err))
		}
		go func() {
			eventingRelay.Run(relayCtx)
			eventingRelayDone <- struct{}{}
		}()
	}
	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "comments-collaboration-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	retentionClient := retention.NewHTTPClient(cfg.RetentionRegistryURL, logger)
	h := handler.New(pgStore, authzClient, logger).WithRetentionClient(retentionClient)
	// No ObjectVisibilityChecker is registered here yet — no other domain
	// service exposes a CanView endpoint for comments-collaboration-svc to
	// call. Until one is, every Mention refuses fail-closed (see
	// handler.ObjectVisibilityChecker's own doc comment) rather than
	// silently allowing. Register one per linked_object_type via
	// h.WithVisibilityChecker(...) as domain services adopt it.

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
		logger.Info("starting comments-collaboration-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down comments-collaboration-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}
	// Stop the relay after the server: requests finishing during Shutdown may still commit
	// events. Anything not delivered stays in the outbox for the next process.
	cancelRelay()
	select {
	case <-eventingRelayDone:
	case <-shutdownCtx.Done():
		logger.Warn("eventing outbox relay did not stop before the shutdown deadline")
	}
	if pool != nil {
		pool.Close()
	}
	logger.Info("server stopped cleanly")
}
