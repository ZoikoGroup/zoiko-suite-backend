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

	"zoiko.io/goods-service-receipt-svc/internal/accounting"
	"zoiko.io/goods-service-receipt-svc/internal/authz"
	"zoiko.io/goods-service-receipt-svc/internal/config"
	"zoiko.io/goods-service-receipt-svc/internal/envelope"
	"zoiko.io/goods-service-receipt-svc/internal/events"
	"zoiko.io/goods-service-receipt-svc/internal/handler"
	"zoiko.io/goods-service-receipt-svc/internal/health"
	"zoiko.io/goods-service-receipt-svc/internal/idempotency"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/outbox"
	"zoiko.io/goods-service-receipt-svc/internal/progress"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
	"zoiko.io/goods-service-receipt-svc/internal/store"
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

	pgStore := store.NewPgStore(pool, logger).WithAccounting(store.AccountingConfig{
		DebitMappingKey: cfg.GRNIDebitMappingKey, CreditMappingKey: cfg.GRNICreditMappingKey,
		PostingPolicyVersion: cfg.PostingPolicyVersion,
	})
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	poClient := purchaseorder.NewHTTPClient(cfg.PurchaseOrderServiceURL, logger)

	// Background workers, all stopped on shutdown: the transactional-outbox relay
	// (ZS-STATE-001 I-13), the AP-03 received-quantity push worker, and the ACC-04
	// GRNI posting dispatcher.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	if pool != nil {
		go outbox.NewRelay(pool, publisher, 500*time.Millisecond, 50, logger).Start(workerCtx)
		if cfg.POProgressPrincipalID == "" {
			logger.Warn("PO_PROGRESS_PRINCIPAL_ID is not set: AP-03 progress worker not started; received-quantity pushes stay PENDING")
		} else {
			go progress.New(pgStore, poClient, cfg.POProgressPrincipalID, 3*time.Second, 50, logger).Start(workerCtx)
		}
		if cfg.AccountingPrincipalID == "" {
			logger.Warn("ACCOUNTING_PRINCIPAL_ID is not set: GRNI posting dispatcher not started; posting requests stay PENDING")
		} else {
			go accounting.New(pgStore, cfg.GeneralLedgerServiceURL, cfg.AccountingPrincipalID, 5*time.Second, 20, logger).Start(workerCtx)
		}
	}

	h := handler.New(pgStore, authzClient, poClient, handler.Config{OverReceiptTolerancePct: cfg.OverReceiptTolerancePct}, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	r.Group(func(r chi.Router) {
		r.Use(middleware.TenantContext())
		// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 section 4): no
		// request reaches business logic without a resolved tenant, actor,
		// correlation and — on material writes — an idempotency key.
		// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
		r.Use(envelope.Middleware(envelope.ServicePolicy(), envelope.DefaultReporter()))
		// Idempotency-Key replay (spec section 16): a repeated confirmation or
		// reversal returns the stored result and never runs twice.
		r.Use(idempotency.Middleware(idempotency.NewPgStore(pool), idempotency.Options{
			TenantFromContext: middleware.TenantFromContext,
			Log:               logger,
		}))
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
		logger.Info("starting goods-service-receipt-svc", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server ListenAndServe error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down goods-service-receipt-svc gracefully...")
	stopWorkers()
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
