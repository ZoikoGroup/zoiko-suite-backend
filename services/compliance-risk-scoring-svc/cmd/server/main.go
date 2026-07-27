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

	var dataStore store.Store

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbPool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Warn("Failed to connect to database, falling back to MemoryStore", zap.Error(err))
		dataStore = store.NewMemoryStore()
	} else if err := dbPool.Ping(ctx); err != nil {
		logger.Warn("Database ping failed, falling back to MemoryStore", zap.Error(err))
		dataStore = store.NewMemoryStore()
	} else {
		logger.Info("Connected to PostgreSQL database")
		dataStore = store.NewPgStore(dbPool)
	}

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer publisher.Close()

	authzClient := authz.NewClient(cfg.AuthzURL, logger)

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

	logger.Info("Server exiting")
}
