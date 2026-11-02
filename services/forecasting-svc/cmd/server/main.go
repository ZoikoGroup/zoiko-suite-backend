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
	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/config"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/handler"
	"zoiko.io/forecasting-svc/internal/mtls"
	"zoiko.io/forecasting-svc/internal/outbox"
	"zoiko.io/forecasting-svc/internal/store"
	"zoiko.io/forecasting-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented to mtls-management-svc
// when provisioning this service's own client-side mTLS identity — the same
// platform-wide scope tax-rules-svc's mTLS pilot uses.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting forecasting-svc", zap.String("port", cfg.Port))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Postgres is the only store. There is no in-memory fallback: a fallback
	// would accept writes that silently vanish on restart and bypass RLS.
	// When the database is down at startup the process still serves /healthz,
	// /readyz reports 503, and business calls fail with 500 until it returns.
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
		logger.Fatal("failed to create database pool", zap.Error(err))
	}
	defer dbPool.Close()
	if err := dbPool.Ping(ctx); err != nil {
		logger.Warn("database ping failed on startup; /readyz will report unavailable", zap.Error(err))
	} else {
		logger.Info("Connected to PostgreSQL database")
	}
	dataStore := store.NewPgStore(dbPool)

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer func() { _ = publisher.Close() }()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "forecasting-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, logger, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}

	h := handler.NewHandler(dataStore, authzClient, logger)
	router := handler.NewRouter(h, dbPool)

	// Transactional outbox relay: store commands write their events into
	// outbox_events in the business transaction; this loop publishes them.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		outbox.NewRelay(dbPool, publisher, time.Second, 50, logger).Run(relayCtx)
	}()

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
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
	stopRelay()
	<-relayDone

	logger.Info("Server exiting")
}
