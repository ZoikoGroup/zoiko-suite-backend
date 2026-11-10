package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"

	"zoiko.io/reconciliation-engine-svc/internal/authz"
	"zoiko.io/reconciliation-engine-svc/internal/config"
	"zoiko.io/reconciliation-engine-svc/internal/handler"
	"zoiko.io/reconciliation-engine-svc/internal/mtls"
	"zoiko.io/reconciliation-engine-svc/internal/source"
	"zoiko.io/reconciliation-engine-svc/internal/store"
	"zoiko.io/reconciliation-engine-svc/internal/telemetry"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync() //nolint:errcheck
	if err := cfg.Validate(); err != nil {
		logger.Fatal("invalid configuration", zap.Error(err))
	}

	logger.Info("Starting reconciliation-engine-svc", zap.String("port", cfg.Port))

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
	dbPool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Fatal("failed to connect to database", zap.Error(err))
	}
	if err := dbPool.Ping(ctx); err != nil {
		logger.Fatal("database ping failed", zap.Error(err))
	}
	logger.Info("Connected to PostgreSQL database")

	// The shared outbox table must exist with the library's exact shape
	// (migration 000012) before anything is enqueued or relayed.
	if err := outbox.VerifySchema(ctx, dbPool); err != nil {
		logger.Fatal("eventing outbox schema check failed", zap.Error(err))
	}
	dataStore := store.NewPgStore(dbPool, store.WithEventRegion(cfg.EventResidencyRegion))

	// The shared relay (services/eventing) delivers eventing_outbox rows:
	// leased claims, bounded retries with backoff, quarantine, acks=all.
	writer, err := kafkaout.New(kafkaout.Config{Brokers: strings.Split(cfg.KafkaBrokers, ","), Topic: cfg.KafkaTopic})
	if err != nil {
		logger.Fatal("eventing outbox writer", zap.Error(err))
	}
	relay, err := outbox.NewRelay(dbPool, writer, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("eventing outbox relay", zap.Error(err))
	}
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.Run(relayCtx)
	}()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "reconciliation-engine-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, logger, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}

	endpoints, err := source.ParseEndpoints(cfg.SourceEndpoints)
	if err != nil {
		logger.Fatal("invalid SOURCE_ENDPOINTS", zap.Error(err))
	}
	if len(endpoints) == 0 {
		logger.Warn("SOURCE_ENDPOINTS is empty: every StartRun will be refused until population sources are registered")
	}

	h := handler.NewHandler(dataStore, authzClient, source.NewHTTPFetcher(endpoints, nil), logger)
	router := handler.NewRouter(h)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 4 * time.Minute, // StartRun reads both populations synchronously; see handler.populationReadTimeout
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	logger.Info("Server listening on port " + cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown", zap.Error(err))
	}

	// Stop the relay after the last request has committed, and let its
	// in-flight pass finish before the pool closes under it.
	stopRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		logger.Warn("eventing outbox relay did not stop before the shutdown deadline")
	}
	writer.Close() //nolint:errcheck
	dbPool.Close()

	logger.Info("Server exiting")
}
