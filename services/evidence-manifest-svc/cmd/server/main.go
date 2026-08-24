// Command server is the entrypoint for evidence-manifest-svc.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	"zoiko.io/evidence-manifest-svc/internal/config"
	svcenvelope "zoiko.io/evidence-manifest-svc/internal/envelope"
	"zoiko.io/evidence-manifest-svc/internal/events"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	"zoiko.io/evidence-manifest-svc/internal/health"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		log.Fatal("failed to parse db pool config", zap.Error(err))
	}
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

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("Postgres unreachable on startup — aborting", zap.Error(err))
	}
	log.Info("Postgres connection established", zap.String("db_name", cfg.DB.Name))

	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	defer func() { _ = kafkaWriter.Close() }()

	pgStore := store.New(pool, log)
	governanceClient := aggregator.NewGovernanceDecisionClient(cfg.GovernanceDecisionLogURL, log)
	accessClient := aggregator.NewAccessDecisionClient(cfg.AuthorizationServiceURL, log)
	workflowClient := aggregator.NewWorkflowClient(cfg.WorkflowServiceURL, log)
	publisher := events.NewPublisher(kafkaWriter, log)

	h := handler.New(pgStore, governanceClient, accessClient, workflowClient, publisher, log)
	healthH := health.New(pool)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", healthH.Readiness)
	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second, // manifest generation fans out to multiple services
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info("evidence-manifest-svc starting", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	<-quit
	log.Info("shutdown signal received")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
}
