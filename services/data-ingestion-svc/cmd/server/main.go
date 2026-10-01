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

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/config"
	"zoiko.io/data-ingestion-svc/internal/events"
	"zoiko.io/data-ingestion-svc/internal/handler"
	"zoiko.io/data-ingestion-svc/internal/mtls"
	"zoiko.io/data-ingestion-svc/internal/outbox"
	"zoiko.io/data-ingestion-svc/internal/store"
	"zoiko.io/data-ingestion-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented to mtls-management-svc
// when provisioning this service's own client-side mTLS identity — same
// platform-wide scope every other service's mTLS pilot uses.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync() //nolint:errcheck

	logger.Info("Starting data-ingestion-svc", zap.String("port", cfg.Port))

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
	dataStore := store.NewPgStore(dbPool)

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer publisher.Close() //nolint:errcheck

	// The outbox Relay is the ONLY path an outbox_events row ever reaches
	// Kafka through — every write in internal/store commits its outbox row
	// in the same transaction as the business change, and this Relay is
	// what turns "durably true in Postgres" into "published," decoupled
	// from whether Kafka happened to be reachable at write time.
	relay := outbox.NewRelay(dbPool, publisher, 5*time.Second, 50, logger)
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	go relay.Start(relayCtx)

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "data-ingestion-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, logger, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}

	h := handler.NewHandler(dataStore, authzClient, logger)
	router := handler.NewRouter(h)

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
	relayCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown", zap.Error(err))
	}

	logger.Info("Server exiting")
}
