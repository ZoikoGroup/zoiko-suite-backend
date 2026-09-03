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

	"zoiko.io/hris-connector-svc/internal/authz"
	"zoiko.io/hris-connector-svc/internal/config"
	svcenvelope "zoiko.io/hris-connector-svc/internal/envelope"
	"zoiko.io/hris-connector-svc/internal/events"
	"zoiko.io/hris-connector-svc/internal/handler"
	"zoiko.io/hris-connector-svc/internal/health"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/mtls"
	"zoiko.io/hris-connector-svc/internal/store"
	"zoiko.io/hris-connector-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	logger, err := telemetry.InitLogger()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	cfg := config.Load()
	logger.Info("starting hris-connector-svc", zap.String("port", cfg.Port))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	dbpool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "hris-connector-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	h := handler.New(st, publisher, authzClient, logger)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.Handler())
	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	logger.Info("hris-connector-svc running", zap.String("addr", srv.Addr))
	<-stop

	logger.Info("shutting down hris-connector-svc...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server forced to shutdown", zap.Error(err))
	}
	logger.Info("hris-connector-svc stopped cleanly")
}
