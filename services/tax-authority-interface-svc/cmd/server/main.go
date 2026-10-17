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

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/eventing/signature"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/config"
	svcenvelope "zoiko.io/tax-authority-interface-svc/internal/envelope"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/handler"
	"zoiko.io/tax-authority-interface-svc/internal/health"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/mtls"
	"zoiko.io/tax-authority-interface-svc/internal/store"
	"zoiko.io/tax-authority-interface-svc/internal/telemetry"
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
	logger.Info("starting tax-authority-interface-svc", zap.String("port", cfg.Port))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Background work (the outbox relay, the sweeper and poller) lives for the life of the process, not for the
	// startup timeout above.
	bg, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

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

	if err := outbox.VerifySchema(ctx, dbpool); err != nil {
		logger.Fatal("eventing outbox schema verification failed", zap.Error(err))
	}

	st := store.NewPgStore(dbpool).WithEventRegion(cfg.EventResidencyRegion)

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	publisher := events.NewKafkaPublisher(brokers, cfg.KafkaEventsTopic, logger)

	writer, err := kafkaout.New(kafkaout.Config{Brokers: brokers, Topic: cfg.KafkaEventsTopic})
	if err != nil {
		logger.Fatal("failed to create kafka writer", zap.Error(err))
	}
	relay, err := outbox.NewRelay(dbpool, writer, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("failed to create outbox relay", zap.Error(err))
	}
	go relay.Run(bg)

	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "tax-authority-interface-svc", platformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		logger.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	h := handler.New(st, publisher, authzClient, logger)

	if cfg.Authority.Enabled {
		engine, err := buildEngine(cfg.Authority, st)
		if err != nil {
			logger.Fatal("authority adapter misconfigured", zap.Error(err))
		}
		cbCfg, err := authority.ParseCallbackConfig(cfg.CallbacksJSON)
		if err != nil {
			logger.Fatal("AUTHORITY_CALLBACKS_JSON is invalid", zap.Error(err))
		}
		var proc *authority.CallbackProcessor
		if len(cbCfg) > 0 {
			proc = &authority.CallbackProcessor{
				Config: cbCfg, Secrets: ext.NewVaultSecrets(cfg.Authority.VaultURL, cfg.Authority.WorkloadID), Store: st,
				Guard: signature.NewMemoryGuard(100_000, nil), Log: logger, TenantCtx: middleware.WithTenant,
			}
		}
		h = h.WithAuthority(engine, proc, st)
		go runWorker(bg, engine, cfg.Authority, logger)
		logger.Info("authority adapter enabled", zap.Int("callback_providers", len(cbCfg)))
	}

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.Handler())
	// The authority callback receiver is machine-authenticated by signature, not by a tenant header, so it sits
	// outside the tenant-context group.
	handler.RegisterCallbackRoute(r, h)

	r.Group(func(r chi.Router) {
		r.Use(middleware.TenantContext)
		// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after Recoverer and telemetry so a
		// refusal is still traced, and ahead of every handler so no request reaches business logic without a
		// resolved tenant, actor, correlation and — on material writes — an idempotency key.
		// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
		r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
		handler.RegisterRoutes(r, h)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	logger.Info("tax-authority-interface-svc running", zap.String("addr", srv.Addr))
	<-stop

	logger.Info("shutting down tax-authority-interface-svc...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server forced to shutdown", zap.Error(err))
	}
	logger.Info("tax-authority-interface-svc stopped cleanly")
}

// buildEngine wires the live adapter: an egress-policy HTTP client (an authority is only ever reached through the
// SSRF guard, whatever a connector says), the connector service, and the vault.
func buildEngine(ac config.AuthorityConfig, st *store.PgStore) (*authority.Engine, error) {
	nets, err := egress.ParseCIDRs(ac.PrivateCIDRs)
	if err != nil {
		return nil, err
	}
	policy := egress.Policy{AllowHTTP: ac.AllowHTTP, AllowedPorts: ac.AllowedPorts, PrivateNets: nets}
	return &authority.Engine{
		Store:      st,
		Connectors: ext.NewConnectorHTTP(ac.ConnectivityURL),
		Creds:      ext.NewVaultHTTP(ac.VaultURL, ac.WorkloadID),
		Transport:  &adapter.Transport{Client: policy.NewHTTPClient(60 * time.Second), MaxBytes: ac.MaxResponse},
		Cfg: authority.Config{
			StaleAfter: ac.StaleAfter, BreakerWindow: ac.BreakerWindow, BreakerFailures: ac.BreakerFailures,
			PollInterval: ac.PollInterval, UrgentPollInterval: ac.UrgentPollInterval,
		},
	}, nil
}

// runWorker releases filings stuck in SUBMITTING (the process died mid-call; never resent) and asks the authority
// about every filing whose poll time has come, more often once its deadline is at risk.
func runWorker(ctx context.Context, e *authority.Engine, ac config.AuthorityConfig, log *zap.Logger) {
	sweep := time.NewTicker(ac.SweepInterval)
	poll := time.NewTicker(ac.PollTick)
	defer sweep.Stop()
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if n, err := e.Sweep(ctx); err != nil {
				log.Error("in-flight sweep failed", zap.Error(err))
			} else if n > 0 {
				log.Warn("released stale in-flight filings to PENDING_UNKNOWN", zap.Int("count", n))
			}
		case <-poll.C:
			if _, err := e.PollDue(ctx, middleware.WithTenant, "tax-authority-poller"); err != nil {
				log.Error("status poll failed", zap.Error(err))
			}
		}
	}
}
