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
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/config"
	"zoiko.io/migration-integrity-svc/internal/events"
	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/mtls"
	"zoiko.io/migration-integrity-svc/internal/store"
	"zoiko.io/migration-integrity-svc/internal/telemetry"
)

// platformScopeID is the platform-wide legal-entity scope used when this
// service provisions its own mTLS client identity from mtls-management-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting migration-integrity-svc", zap.String("port", cfg.Port))

	var dataStore store.Store

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
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil || pool.Ping(ctx) != nil {
		logger.Warn("Database unavailable, using MemoryStore")
		dataStore = store.NewMemoryStore()
	} else {
		logger.Info("Connected to PostgreSQL")
		dataStore = store.NewPgStore(pool)
	}

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer publisher.Close()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "migration-integrity-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}
	h := handler.NewHandler(dataStore, publisher, authzClient, logger)
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
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	logger.Info("Server listening on :" + cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	logger.Info("Server exited")
}
