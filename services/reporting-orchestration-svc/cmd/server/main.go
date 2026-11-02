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

	// Step 1: Postgres is mandatory. This service used to fall back to an
	// in-memory store when the database was down, silently accepting
	// regulated report definitions and runs it would lose on restart and
	// skipping RLS, idempotency and the outbox. It now refuses to start.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	pool := connectPostgres(startupCtx, cfg.DatabaseURL, logger)
	defer pool.Close()
	pgStore := store.NewPgStore(pool)

	// Step 2: Kafka publisher + transactional-outbox relay.
	publisher := events.NewPublisher(strings.Split(cfg.KafkaBrokers, ","), cfg.KafkaTopic, logger)
	defer publisher.Close()
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go events.NewRelay(pgStore, publisher, logger).Run(relayCtx)

	// Step 3: upstream clients (all fail closed).
	authzClient := newAuthzClient(startupCtx, cfg, logger)
	archiveClient := archivestore.NewHTTPClient(cfg.AuditEventStoreURL, logger)
	retentionClient := retention.NewHTTPClient(cfg.RetentionRegistryURL, logger)

	// Step 4: HTTP handler and router.
	h := handler.NewHandler(handler.Dependencies{
		Definitions: pgStore,
		Runs:        pgStore,
		Exports:     pgStore,
		Idempotency: pgStore,
		Archives:    archiveClient,
		Retentions:  retentionClient,
		Authz:       authzClient,
		Ready:       pool,
		Logger:      logger,
	})
	srv := &http.Server{
		Addr:         cfg.ListenHost + ":" + cfg.Port,
		Handler:      handler.NewRouter(h),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("Server listening", zap.String("addr", srv.Addr))

	// Step 5: graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	stopRelay()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("Server exited")
}

// connectPostgres opens the pool and verifies connectivity, or exits.
func connectPostgres(ctx context.Context, databaseURL string, logger *zap.Logger) *pgxpool.Pool {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
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
		logger.Fatal("failed to create db pool", zap.Error(err))
	}
	if err := pool.Ping(ctx); err != nil {
		logger.Fatal("database unavailable — refusing to start without Postgres", zap.Error(err))
	}
	logger.Info("Connected to PostgreSQL")
	return pool
}

// newAuthzClient builds the authorization-svc client (mTLS when enabled).
func newAuthzClient(ctx context.Context, cfg *config.Config, logger *zap.Logger) *authz.Client {
	if !cfg.AuthzMTLSEnabled {
		return authz.NewClient(cfg.AuthzURL, logger)
	}
	mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "reporting-orchestration-svc", platformScopeID)
	if err != nil {
		logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
	}
	logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
	return authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
}
