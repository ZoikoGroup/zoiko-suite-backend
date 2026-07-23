package main

import (
	"context"
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

	"zoiko.io/external-data-feed-svc/internal/authz"
	"zoiko.io/external-data-feed-svc/internal/config"
	"zoiko.io/external-data-feed-svc/internal/events"
	"zoiko.io/external-data-feed-svc/internal/handler"
	"zoiko.io/external-data-feed-svc/internal/health"
	"zoiko.io/external-data-feed-svc/internal/middleware"
	"zoiko.io/external-data-feed-svc/internal/store"
	"zoiko.io/external-data-feed-svc/internal/telemetry"
)

func main() {
	logger, err := telemetry.InitLogger()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	cfg := config.Load()
	logger.Info("starting external-data-feed-svc", zap.String("port", cfg.Port))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbpool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("unable to connect to database", zap.Error(err))
		os.Exit(1)
	}
	defer dbpool.Close()

	if err := dbpool.Ping(ctx); err != nil {
		logger.Warn("database ping failed on startup", zap.Error(err))
	} else {
		logger.Info("connected to postgres database")
	}

	st := store.NewPgStore(dbpool)
	publisher := events.NewKafkaPublisher(strings.Split(cfg.KafkaBrokers, ","), cfg.KafkaEventsTopic, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	h := handler.New(st, publisher, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext)

	r.Get("/healthz", health.Handler())
	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr: ":" + cfg.Port, Handler: r,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	logger.Info("external-data-feed-svc running", zap.String("addr", srv.Addr))
	<-stop

	logger.Info("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server forced to shutdown", zap.Error(err))
	}
	logger.Info("external-data-feed-svc stopped cleanly")
}
