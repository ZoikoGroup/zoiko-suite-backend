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
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/bankrec"
	"zoiko.io/reconciliation-intelligence-svc/internal/config"
	"zoiko.io/reconciliation-intelligence-svc/internal/financialcontrol"
	"zoiko.io/reconciliation-intelligence-svc/internal/handler"
	"zoiko.io/reconciliation-intelligence-svc/internal/ledger"
	"zoiko.io/reconciliation-intelligence-svc/internal/mtls"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
	"zoiko.io/reconciliation-intelligence-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger, err := telemetry.NewLogger(cfg.LogLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	logger.Info("Starting reconciliation-intelligence-svc", zap.String("port", cfg.Port))

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

	// Verify connectivity at startup — fail fast rather than silently degrade.
	// This service holds human decisions (APPROVED/REJECTED/EXECUTED); an
	// in-memory fallback would lose every one of them on the next restart,
	// with no error anyone would see. Refusing to start is the only safe
	// answer to an unreachable database.
	if err := dbPool.Ping(ctx); err != nil {
		logger.Fatal("db unreachable at startup", zap.Error(err))
	}
	logger.Info("db pool connected")

	if err := outbox.VerifySchema(ctx, dbPool); err != nil {
		logger.Fatal("eventing outbox schema missing", zap.Error(err))
	}
	dataStore := store.NewPgStore(dbPool, store.WithEventRegion(cfg.EventResidencyRegion))

	// Reconciliation lifecycle events are written to eventing_outbox in
	// the same transaction as the job/resolution change; this relay
	// delivers them. kafkaout requires broker acknowledgement
	// (RequireAll) — a fire-and-forget writer here would turn a
	// "published" event into one no broker stored, the exact loss the
	// outbox exists to prevent.
	outboxWriter, err := kafkaout.New(kafkaout.Config{
		Brokers:                strings.Split(cfg.KafkaBrokers, ","),
		Topic:                  cfg.KafkaTopic,
		AllowAutoTopicCreation: true,
	})
	if err != nil {
		logger.Fatal("outbox kafka writer", zap.Error(err))
	}
	defer func() { _ = outboxWriter.Close() }()
	relay, err := outbox.NewRelay(dbPool, outboxWriter, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("outbox relay", zap.Error(err))
	}
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relayDone := make(chan struct{}, 1)
	go func() {
		relay.Run(relayCtx)
		relayDone <- struct{}{}
	}()

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "reconciliation-intelligence-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, logger)
	} else {
		authzClient = authz.NewClient(cfg.AuthzURL, logger)
	}

	financialControlClient := financialcontrol.NewClient(cfg.FinancialControlURL, logger)
	bankRecClient := bankrec.NewClient(cfg.BankReconciliationURL, logger)

	ledgerClient := ledger.NewClient(cfg.GeneralLedgerURL, logger)

	h := handler.NewHandler(dataStore, authzClient, financialControlClient, bankRecClient, ledgerClient, cfg.CandidateSearchHorizonDays, logger)
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

	// Stop the relay after the server: requests finishing during Shutdown
	// may still commit events. Anything not delivered stays in the outbox
	// for the next process — stopping loses nothing.
	cancelRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		logger.Warn("outbox relay did not stop before the shutdown deadline")
	}

	logger.Info("Server exiting")
}
