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
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"zoiko.io/reporting-orchestration-svc/internal/archivestore"
	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/config"
	"zoiko.io/reporting-orchestration-svc/internal/events"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
	"zoiko.io/reporting-orchestration-svc/internal/mtls"
	"zoiko.io/reporting-orchestration-svc/internal/retention"
	"zoiko.io/reporting-orchestration-svc/internal/store"
	"zoiko.io/reporting-orchestration-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg := config.Load()

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting reporting-orchestration-svc", zap.String("port", cfg.Port))

	var dataStore store.Store
	// pgStore is kept separately (not just as the store.Store interface
	// value above) because AUD-10's export routes need the concrete
	// *store.PgStore's ExportStore methods — MemoryStore is a
	// DB-unavailable fallback that exists only for the pre-existing report
	// endpoints; approval/seal/deliver correctness genuinely depends on
	// real transactional CAS predicates, so there is no in-memory
	// equivalent to fall back to. When the DB is unavailable, AUD-10's
	// routes are simply not registered (see below) rather than silently
	// running against a fake store.
	var pgStore *store.PgStore

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
		pgStore = store.NewPgStore(pool)
		dataStore = pgStore
	}

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewPublisher(brokers, cfg.KafkaTopic, logger)
	defer publisher.Close()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "reporting-orchestration-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}
	h := handler.NewHandler(dataStore, publisher, authzClient, logger)
	router := handler.NewRouter(h)

	// AUD-10 export/redact/deliver routes — only mounted when a real
	// Postgres connection is available (see pgStore's own comment above).
	if pgStore != nil {
		archiveClient := archivestore.NewHTTPClient(cfg.AuditEventStoreURL, logger)
		retentionClient := retention.NewHTTPClient(cfg.RetentionRegistryURL, logger)
		if chiRouter, ok := router.(chi.Router); ok {
			handler.RegisterExportRoutes(chiRouter, h, pgStore, archiveClient, retentionClient)
		} else {
			logger.Error("router does not implement chi.Router — AUD-10 export routes not mounted")
		}
	} else {
		logger.Warn("Database unavailable — AUD-10 export routes not mounted")
	}

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
