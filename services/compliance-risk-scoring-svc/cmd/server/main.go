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
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/config"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/handler"
	"zoiko.io/compliance-risk-scoring-svc/internal/mtls"
	"zoiko.io/compliance-risk-scoring-svc/internal/outbox"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
	"zoiko.io/compliance-risk-scoring-svc/internal/telemetry"
)

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting compliance-risk-scoring-svc", zap.String("port", cfg.Port))

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
		logger.Fatal("failed to create db pool", zap.Error(err))
	}
	defer dbPool.Close()
	// No in-memory fallback: a scoring service without its database cannot
	// keep evidence. An unreachable database only makes /readyz report 503.
	if err := dbPool.Ping(ctx); err != nil {
		logger.Warn("database ping failed at startup; /readyz will report unavailable until it recovers", zap.Error(err))
	} else {
		logger.Info("Connected to PostgreSQL database")
	}
	dataStore := store.New(dbPool)

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer publisher.Close()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "compliance-risk-scoring-svc", "00000000-0000-0000-0000-00000000f001")
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}

	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go outbox.NewRelay(dataStore, publisher, logger, 2*time.Second).Run(relayCtx)

	svc := service.New(dataStore, authzClient, logger)
	h := handler.NewHandler(svc, dataStore, logger)
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
	stopRelay()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown", zap.Error(err))
	}

	logger.Info("Server exiting")
}
