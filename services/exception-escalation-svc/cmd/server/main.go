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

	"zoiko.io/eventing/kafkaout"
	eventingoutbox "zoiko.io/eventing/outbox"
	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/config"
	svcenvelope "zoiko.io/exception-escalation-svc/internal/envelope"
	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/handler"
	"zoiko.io/exception-escalation-svc/internal/health"
	"zoiko.io/exception-escalation-svc/internal/idempotency"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/mtls"
	"zoiko.io/exception-escalation-svc/internal/outbox"
	"zoiko.io/exception-escalation-svc/internal/store"
	"zoiko.io/exception-escalation-svc/internal/telemetry"
)

func main() {
	logger, err := telemetry.NewLogger("exception-escalation-svc")
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

	var pool *pgxpool.Pool
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		logger.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
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
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)

	// Relay for the BIZ-08 deadline events on the shared eventing outbox. The other events keep
	// the direct publisher. With no brokers the relay is not started: events queue in eventing_outbox.
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	eventingRelayDone := make(chan struct{}, 1)
	if pool == nil || cfg.KafkaBrokers == "" {
		logger.Warn("no database or KAFKA_BROKERS: the eventing outbox relay is not running; BIZ-08 deadline events will queue in eventing_outbox")
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

	const platformScopeID = "00000000-0000-0000-0000-00000000f001"
	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "exception-escalation-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	h := handler.New(pgStore, authzClient, logger)

	// Transactional outbox relay: every write enqueues its event in the same
	// transaction as the change; the relay delivers it with retry.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	if pool != nil {
		go outbox.NewRelay(pool, publisher, 0, 0, logger).Start(relayCtx)
	}

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContextMiddleware)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// Replay protection for material writes: a retried POST/PUT with the same
	// Idempotency-Key gets the first answer instead of running twice. Placed after
	// the envelope middleware, which guarantees the key and tenant are present.
	r.Use(idempotency.Middleware(idempotency.NewPgStore(pool), logger))

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	handler.RegisterRoutes(r, h)
	handler.RegisterFindingRoutes(r, h, pgStore)
	handler.RegisterTaskRoutes(r, h, pgStore)
	handler.RegisterDeadlineRoutes(r, h, pgStore)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting exception-escalation-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down exception-escalation-svc gracefully...")
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
