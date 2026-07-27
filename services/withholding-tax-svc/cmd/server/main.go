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

	"zoiko.io/withholding-tax-svc/internal/authz"
	"zoiko.io/withholding-tax-svc/internal/config"
	"zoiko.io/withholding-tax-svc/internal/events"
	"zoiko.io/withholding-tax-svc/internal/handler"
	"zoiko.io/withholding-tax-svc/internal/health"
	"zoiko.io/withholding-tax-svc/internal/middleware"
	"zoiko.io/withholding-tax-svc/internal/store"
	"zoiko.io/withholding-tax-svc/internal/telemetry"
)

func main() {
	logger, err := telemetry.NewLogger("withholding-tax-svc")
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

	var pool *pgxpool.Pool
	pool, err = pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		logger.Warn("unable to connect to database on startup", zap.Error(err))
	} else {
		logger.Info("connected to postgres database")
	}

	pgStore := store.NewPgStore(pool)
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)

	h := handler.New(pgStore, publisher, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContextMiddleware)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting withholding-tax-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down withholding-tax-svc gracefully...")
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
