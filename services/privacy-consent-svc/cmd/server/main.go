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
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/authz"
	"zoiko.io/privacy-consent-svc/internal/config"
	"zoiko.io/privacy-consent-svc/internal/handler"
	"zoiko.io/privacy-consent-svc/internal/health"
	"zoiko.io/privacy-consent-svc/internal/middleware"
	"zoiko.io/privacy-consent-svc/internal/outbox"
	"zoiko.io/privacy-consent-svc/internal/purposeregistry"
	"zoiko.io/privacy-consent-svc/internal/store"
)

// openPool builds the pool AND proves the database answers. pgxpool's
// constructor is lazy — it returns a usable-looking pool without having
// opened a connection — so without the Ping an unreachable database was not
// discovered until the first request. Never returns a non-nil pool with a
// non-nil error.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse db pool config: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

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

	// A failed startup connection used to be logged at WARN and the process
	// carried on with a nil *pgxpool.Pool: every store call and the outbox
	// relay goroutine then dereferenced it and panicked. Every route needs
	// the database, so there is nothing useful a pool-less process can
	// serve — abort and let the orchestrator restart it (compose:
	// restart: on-failure), same as every other service that owns a store.
	pool, err := openPool(ctx, cfg.DSN())
	if err != nil {
		logger.Fatal("database unreachable on startup — aborting", zap.Error(err))
	}
	logger.Info("connected to postgres database")

	pgStore := store.NewPgStore(pool, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	purposeClient := purposeregistry.NewClient(cfg.PurposeRegistryURL)

	h := handler.New(pgStore, authzClient, purposeClient, logger)

	// ── Event outbox relay (ZS-EVENT-001 §6/§7) ───────────────────────────────
	//
	// Replaces the old events.KafkaPublisher, which was called AFTER each
	// store transaction had already committed: a crash or a broker outage
	// between those two steps lost the event silently, with only a WARN log
	// line ("event dropped") as the failure handling. The store now enqueues
	// every event inside its own transaction (internal/outbox.Enqueue); this
	// relay is the only thing that ever talks to Kafka for it, and it
	// retries with backoff until the broker actually acknowledges delivery.
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  cfg.KafkaEventsTopic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		// RequiredAcks defaults to fire-and-forget in kafka-go, which would
		// let the relay mark an event published before any broker actually
		// stored it — reintroducing, one layer down, the exact loss this
		// outbox exists to prevent.
		RequiredAcks: kafka.RequireAll,
	}
	defer func() { _ = kafkaWriter.Close() }()
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	if pool != nil {
		relay := outbox.NewRelay(pool, kafkaWriter, logger)
		go relay.Run(relayCtx, 2*time.Second)
	}

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	r.Group(func(r chi.Router) {
		r.Use(middleware.TenantContext())
		handler.RegisterRoutes(r, h)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting privacy-consent-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down privacy-consent-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}
	// Stop the relay before closing the pool it reads from. Anything left
	// unpublished stays durable in event_outbox for the next process to
	// pick up — nothing is lost by stopping here.
	cancelRelay()
	if pool != nil {
		pool.Close()
	}
	logger.Info("server stopped cleanly")
}
