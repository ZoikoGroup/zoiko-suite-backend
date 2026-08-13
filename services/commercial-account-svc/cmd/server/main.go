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

	"zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/config"
	"zoiko.io/commercial-account-svc/internal/events"
	"zoiko.io/commercial-account-svc/internal/handler"
	"zoiko.io/commercial-account-svc/internal/health"
	"zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/outbox"
	"zoiko.io/commercial-account-svc/internal/store"
	"zoiko.io/commercial-account-svc/internal/telemetry"
)

func main() {
	logger, err := telemetry.NewLogger("commercial-account-svc")
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

	pgStore := store.NewPgStore(pool)
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)

	h := handler.New(pgStore, publisher, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext())

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	handler.RegisterRoutes(r, h)
	handler.RegisterSubscriptionRoutes(r, h)

	// Outbox relay (doc7 backlog item 32 pilot): publishes rows written by
	// PgStore.CreateSubscription in the same transaction as the business
	// write, decoupling "the fact is durable" from "Kafka was reachable at
	// that exact moment." Runs until relayCancel is called at shutdown.
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	relay := outbox.NewRelay(pool, publisher, 5*time.Second, 50, logger)
	go relay.Start(relayCtx)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting commercial-account-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down commercial-account-svc gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown forced", zap.Error(err))
	}
	if pool != nil {
		pool.Close()
	}
	logger.Info("server stopped cleanly")
}
