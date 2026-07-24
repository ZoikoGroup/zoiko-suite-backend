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
	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/config"
	"zoiko.io/reporting-orchestration-svc/internal/events"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
	"zoiko.io/reporting-orchestration-svc/internal/store"
	"zoiko.io/reporting-orchestration-svc/internal/telemetry"
)

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting reporting-orchestration-svc", zap.String("port", cfg.Port))

	var dataStore store.Store

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
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
