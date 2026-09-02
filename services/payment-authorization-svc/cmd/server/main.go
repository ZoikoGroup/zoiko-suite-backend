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

	"zoiko.io/payment-authorization-svc/internal/authz"
	"zoiko.io/payment-authorization-svc/internal/config"
	"zoiko.io/payment-authorization-svc/internal/events"
	"zoiko.io/payment-authorization-svc/internal/handler"
	"zoiko.io/payment-authorization-svc/internal/health"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/payeeidentity"
	"zoiko.io/payment-authorization-svc/internal/paymentproposal"
	"zoiko.io/payment-authorization-svc/internal/policy"
	"zoiko.io/payment-authorization-svc/internal/store"
	"zoiko.io/payment-authorization-svc/internal/supplierprofile"
)

func main() {
	logger, err := zap.NewProduction()
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

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		logger.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Warn("unable to connect to database on startup", zap.Error(err))
	} else {
		logger.Info("connected to postgres database")
	}

	pgStore := store.NewPgStore(pool, logger)
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	proposalClient := paymentproposal.NewHTTPClient(cfg.PaymentProposalServiceURL, logger)
	supplierClient := supplierprofile.NewHTTPClient(cfg.SupplierProfileServiceURL, logger)
	payeeClient := payeeidentity.NewHTTPClient(cfg.PayeeIdentityServiceURL, logger)
	policyClient := policy.NewHTTPClient(cfg.PolicyServiceURL, logger)

	h := handler.New(pgStore, publisher, authzClient, proposalClient, supplierClient, payeeClient, policyClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	r.Group(func(r chi.Router) {
		r.Use(middleware.TenantContext())
		handler.RegisterRoutes(r, h)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("starting payment-authorization-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down payment-authorization-svc gracefully...")
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
