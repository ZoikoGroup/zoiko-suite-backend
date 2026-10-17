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
	"zoiko.io/eventing/outbox"
	"zoiko.io/eventing/signature"
	"zoiko.io/payment-status-svc/internal/authz"
	"zoiko.io/payment-status-svc/internal/callbacks"
	"zoiko.io/payment-status-svc/internal/config"
	"zoiko.io/payment-status-svc/internal/handler"
	"zoiko.io/payment-status-svc/internal/health"
	"zoiko.io/payment-status-svc/internal/middleware"
	"zoiko.io/payment-status-svc/internal/store"
)

func main() {
	logger, err := zap.NewProduction()
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

	// The eventing outbox table must exist before the first business write
	// tries to enqueue into it; fail here, at deploy time, not on a request.
	if err := outbox.VerifySchema(ctx, pool); err != nil {
		logger.Fatal("eventing outbox schema missing", zap.Error(err))
	}

	pgStore := store.NewPgStore(pool, logger, store.WithEventRegion(cfg.EventResidencyRegion))

	// Transactional outbox relay (ZS-EVENT-001 §6). Every BNK-07 event is
	// written to eventing_outbox in the same transaction as the state
	// change it reports; this relay delivers them. kafkaout builds its own
	// writer because delivery must be acknowledged (RequireAll) — a
	// fire-and-forget writer must never back the outbox, or a "published"
	// event could be one no broker actually stored.
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	outboxWriter, err := kafkaout.New(kafkaout.Config{
		Brokers: brokers,
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
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()

	authzClient := authz.NewClient(cfg.AuthzServiceURL)

	h := handler.New(pgStore, authzClient, cfg.WebhookSharedSecret, logger)

	provCfg, err := callbacks.ParseConfig(cfg.ProviderCallbacksJSON)
	if err != nil {
		logger.Fatal("PROVIDER_CALLBACKS_JSON is invalid", zap.Error(err))
	}
	if len(provCfg) > 0 {
		h = h.WithCallbacks(&callbacks.Processor{
			Config:  provCfg,
			Secrets: callbacks.NewVaultSecrets(cfg.VaultURL, cfg.VaultWorkloadID),
			Store:   pgStore,
			Guard:   signature.NewMemoryGuard(100_000, nil),
			Log:     logger,
		}, pgStore)
		logger.Info("provider callbacks enabled", zap.Int("providers", len(provCfg)))
	}

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	// The webhook route is registered outside the tenant-context group —
	// see internal/handler's RegisterRoutes doc comment — everything else
	// keeps the usual tenant scoping.
	handler.RegisterRoutes(r, h, middleware.TenantContext())

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting payment-status-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down payment-status-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}

	// Stop the relay after the server: requests finishing during Shutdown
	// may still commit events. Anything not delivered stays in the outbox
	// for the next process — stopping loses nothing.
	cancelRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		logger.Warn("outbox relay did not stop before the shutdown deadline")
	}

	if pool != nil {
		pool.Close()
	}
	logger.Info("server stopped cleanly")
}
