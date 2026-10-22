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

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/config"
	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	svcenvelope "zoiko.io/connectivity-api-bridge-svc/internal/envelope"
	"zoiko.io/connectivity-api-bridge-svc/internal/handler"
	"zoiko.io/connectivity-api-bridge-svc/internal/health"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/mtls"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/telemetry"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
)

func main() {
	logger, err := telemetry.InitLogger()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	cfg := config.Load()
	logger.Info("starting connectivity-api-bridge-svc", zap.String("port", cfg.Port))

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

	// A service whose migration was never applied would start normally and fail
	// on the first request that enqueues an event. Fail here, at deploy time.
	if err := outbox.VerifySchema(ctx, dbpool); err != nil {
		logger.Fatal("eventing outbox schema missing — apply migration 003_connector_management.sql", zap.Error(err))
	}

	// The contract registry (XIC-01) is migration 004. Fail at deploy time, not on the first contract request.
	var registryTable *string
	if err := dbpool.QueryRow(ctx, `SELECT to_regclass('public.xic_contract_versions')::text`).Scan(&registryTable); err != nil || registryTable == nil {
		logger.Fatal("contract registry schema missing — apply migration 004_xic_registry.sql", zap.Error(err))
	}
	var connTable *string
	if err := dbpool.QueryRow(ctx, `SELECT to_regclass('public.xic_connections')::text`).Scan(&connTable); err != nil || connTable == nil {
		logger.Fatal("connection schema missing — apply migration 005_xic_connections.sql", zap.Error(err))
	}
	var inboundTable *string
	if err := dbpool.QueryRow(ctx, `SELECT to_regclass('public.xic_inbound_exchanges')::text`).Scan(&inboundTable); err != nil || inboundTable == nil {
		logger.Fatal("inbound schema missing — apply migration 006_xic_inbound.sql", zap.Error(err))
	}

	st := store.NewPgStore(dbpool, store.WithEventRegion(cfg.EventResidencyRegion))

	// Transactional outbox relay. Events are written in the same transaction as
	// the state change they describe and delivered from here, so a crash or a
	// broker outage between commit and publish can no longer lose one.
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	writer, err := kafkaout.New(kafkaout.Config{Brokers: brokers, Topic: cfg.KafkaEventsTopic, AllowAutoTopicCreation: true})
	if err != nil {
		logger.Fatal("kafka writer", zap.Error(err))
	}
	defer func() { _ = writer.Close() }()
	relay, err := outbox.NewRelay(dbpool, writer, outbox.DefaultConfig(), logger)
	if err != nil {
		logger.Fatal("outbox relay", zap.Error(err))
	}
	relay.SetRegion(cfg.EventResidencyRegion)
	gate, err := schemaGate(cfg)
	if err != nil {
		logger.Fatal("schema gate", zap.Error(err))
	}
	if gate != nil {
		relay.SetGate(gate)
		logger.Info("producer schema gate enabled", zap.String("registry", cfg.SchemaRegistryURL), zap.Bool("allow_unregistered", cfg.SchemaGateAllowUnregistered))
	}
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go relay.Run(relayCtx)

	var authzClient *authz.Client
	var mtlsHTTPClient *http.Client
	if cfg.AuthzMTLSEnabled || cfg.VaultMTLSEnabled {
		mtlsHTTPClient, err = mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "connectivity-api-bridge-svc", store.PlatformScopeID)
		if err != nil {
			logger.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
	}
	if cfg.AuthzMTLSEnabled {
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient)
	} else {
		authzClient = authz.NewClient(cfg.AuthzServiceURL)
	}

	vaultURL, vaultHTTP := cfg.VaultServiceURL, (*http.Client)(nil)
	if cfg.VaultMTLSEnabled {
		vaultURL, vaultHTTP = cfg.VaultMTLSURL, mtlsHTTPClient
	}
	vaultClient, err := vault.New(vaultURL, vaultHTTP)
	if err != nil {
		logger.Fatal("secret vault client", zap.Error(err))
	}

	if len(cfg.EgressAllowedRegions) == 0 {
		logger.Warn("EGRESS_ALLOWED_REGIONS is empty: no connector can be submitted or approved until a region is allowed")
	}
	svc := &connector.Service{
		Store: st, Vault: vaultClient,
		Egress:               egressPolicy(cfg, logger),
		EgressAllowedRegions: cfg.EgressAllowedRegions,
	}
	h := handler.New(svc, st, authzClient, logger)

	regRepo := store.NewRegistryRepo(st)
	regSvc := &registry.Service{Repo: regRepo, Limits: registry.Limits{
		MaxPayloadBytes: cfg.XICMaxPayloadBytes, IdempotencyMinDays: cfg.XICIdempotencyMinDays,
		IdempotencyMaxDays: cfg.XICIdempotencyMaxDays, CertificationMaxDays: cfg.XICCertificationMaxDays,
	}}
	rh := handler.NewRegistry(regSvc, regRepo, authzClient, logger)

	connRepo := store.NewXICConnRepo(st)
	connSvc := &xicconn.Service{
		Repo: connRepo, Contracts: regRepo, Vault: vaultClient, Egress: egressPolicy(cfg, logger),
		AllowedRegions: cfg.EgressAllowedRegions, RedirectAllowlist: cfg.XICRedirectAllowlist,
		Cfg: xicconn.Config{
			AuthSessionTTL: time.Duration(cfg.XICAuthSessionMinutes) * time.Minute, ExpiryWarning: time.Duration(cfg.XICExpiryWarningDays) * 24 * time.Hour,
			MaxGrace: time.Duration(cfg.XICMaxGraceHours) * time.Hour, RotationInterval: time.Duration(cfg.XICRotationDays) * 24 * time.Hour,
			BreakGlassMax: time.Duration(cfg.XICBreakGlassMaxMinutes) * time.Minute,
		},
	}
	ch := handler.NewConnection(connSvc, connRepo, authzClient, logger)
	evalCtx, stopEval := context.WithCancel(context.Background())
	defer stopEval()
	go connSvc.RunEvaluator(evalCtx, time.Duration(cfg.XICEvaluatorIntervalSecs)*time.Second, 200, func(n int, err error) {
		if err != nil {
			logger.Warn("connection evaluator", zap.Int("due", n), zap.Error(err))
		}
	})

	// XIC-03 inbound gateway (ZS-SVC-T-001 section 8): the handoff worker, the management API and, when enabled, the
	// public ingest endpoint. Credential material comes from the vault through its narrow use operation.
	inRepo := store.NewInboundRepo(st)
	gw := &inbound.Service{
		Repo: inRepo, Conns: connSvc, Secrets: inbound.NewVaultSecrets(cfg.VaultServiceURL, "connectivity-api-bridge-svc"), EdgeSecret: []byte(cfg.XICEdgeSecret),
		Cfg: inbound.Config{MaxBodyBytes: cfg.XICInboundMaxBodyBytes, RawRetention: time.Duration(cfg.XICRawRetentionDays) * 24 * time.Hour, RawMaxBytes: cfg.XICRawMaxBytes,
			RawDenyClasses: cfg.XICRawDenyClasses, HandoffMaxAttempts: cfg.XICHandoffMaxAttempts, HandoffBackoffBase: 5 * time.Second, HandoffBackoffMax: 15 * time.Minute,
			HandoffLease: time.Minute, ConnRatePerMin: cfg.XICConnRatePerMin, IPRatePerMin: cfg.XICIPRatePerMin},
		Handoff: inbound.HandoffDeps{Egress: egressPolicy(cfg, logger), AllowedRegions: cfg.EgressAllowedRegions, Principal: "system:xic-inbound-gateway"},
	}
	inAdmin := handler.NewInboundAdmin(gw, inRepo, authzClient, logger)
	go gw.RunHandoff(evalCtx, time.Duration(cfg.XICHandoffIntervalSecs)*time.Second, 100, func(n int, err error) {
		if err != nil {
			logger.Warn("inbound handoff", zap.Int("due", n), zap.Error(err))
		}
	})

	// api carries every ZoikoSuite-authenticated route behind the tenant and envelope middleware. The ingest endpoint
	// is mounted beside it, NOT inside it: an external sender has no tenant header, and its identity is proven by the
	// contract's authentication profile, never by headers it could set itself.
	r := chi.NewRouter()
	r.Use(chimiddleware.RealIP)
	r.Use(middleware.TenantContext)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.Handler())
	handler.RegisterRoutes(r, h)
	handler.RegisterRegistryRoutes(r, rh)
	handler.RegisterConnectionRoutes(r, ch)
	handler.RegisterInboundAdminRoutes(r, inAdmin)

	root := chi.NewRouter()
	root.Use(chimiddleware.RequestID)
	root.Use(chimiddleware.Logger)
	root.Use(chimiddleware.Recoverer)
	if cfg.XICInboundEnabled {
		root.Mount("/v1/inbound", handler.NewIngest(gw, cfg.XICInboundMaxBodyBytes, cfg.XICInboundTrustXFF, logger).Router())
		logger.Info("inbound ingest endpoint enabled")
	}
	root.Mount("/", r)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      root,
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

	logger.Info("connectivity-api-bridge-svc running", zap.String("addr", srv.Addr))
	<-stop

	logger.Info("shutting down connectivity-api-bridge-svc...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server forced to shutdown", zap.Error(err))
	}
	logger.Info("connectivity-api-bridge-svc stopped cleanly")
}

// egressPolicy builds the connector egress rules. A malformed private range stops the service: an
// allowlist that silently parses to nothing would hide a misconfiguration.
func egressPolicy(cfg *config.Config, logger *zap.Logger) egress.Policy {
	nets, err := egress.ParseCIDRs(cfg.EgressPrivateCIDRs)
	if err != nil {
		logger.Fatal("EGRESS_PRIVATE_CIDRS is invalid", zap.Error(err))
	}
	return egress.Policy{AllowHTTP: cfg.EgressAllowInsecureHTTP, AllowedPorts: cfg.EgressAllowedPorts, PrivateNets: nets}
}
