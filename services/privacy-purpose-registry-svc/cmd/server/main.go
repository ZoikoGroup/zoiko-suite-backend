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

	"zoiko.io/privacy-purpose-registry-svc/internal/authz"
	"zoiko.io/privacy-purpose-registry-svc/internal/config"
	"zoiko.io/privacy-purpose-registry-svc/internal/events"
	"zoiko.io/privacy-purpose-registry-svc/internal/handler"
	"zoiko.io/privacy-purpose-registry-svc/internal/health"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
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
	// pgxpool's constructor is lazy: it returns a usable-looking pool without
	// opening a connection, and the old warn-and-continue left a nil pool that
	// every store call and the outbox relay would dereference. Every route
	// needs the database, so a pool-less process serves nothing useful: prove
	// the database answers, or abort and let the orchestrator restart us.
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Fatal("unable to create database pool — aborting", zap.Error(err))
	}
	if err := pool.Ping(ctx); err != nil {
		logger.Fatal("database unreachable on startup — aborting", zap.Error(err))
	}
	logger.Info("connected to postgres database")

	pgStore := store.NewPgStore(pool, logger)
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	authzClient := authz.NewClient(cfg.AuthzServiceURL)

	// The transactional outbox is the ONLY event path: the store enqueues
	// inside the transaction that changes state; this relay is the only thing
	// that talks to Kafka, retrying with backoff until a broker acknowledges.
	kafkaWriter := events.NewKafkaWriter(brokers, cfg.KafkaEventsTopic)
	defer func() { _ = kafkaWriter.Close() }()
	relay := events.NewOutboxRelay(pool, kafkaWriter, cfg.KafkaEventsTopic, logger)
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	go relay.Start(relayCtx, 3*time.Second)

	h := handler.New(pgStore, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	// /healthz and /readyz stay OUTSIDE the tenant gate — liveness/readiness
	// probes carry no tenant, same doctrine as every other service in this
	// platform.
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
		logger.Info("starting privacy-purpose-registry-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down privacy-purpose-registry-svc gracefully...")
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
