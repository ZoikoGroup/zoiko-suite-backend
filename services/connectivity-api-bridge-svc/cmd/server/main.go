package main

import (
	"context"
	"encoding/json"
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
	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/config"
	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	svcenvelope "zoiko.io/connectivity-api-bridge-svc/internal/envelope"
	"zoiko.io/connectivity-api-bridge-svc/internal/handler"
	"zoiko.io/connectivity-api-bridge-svc/internal/health"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/mtls"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/sftppull"
	"zoiko.io/connectivity-api-bridge-svc/internal/signals"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/telemetry"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/kafkaout"
	"zoiko.io/eventing/outbox"
	"zoiko.io/eventing/scan"
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

	var batchTable *string
	if err := dbpool.QueryRow(ctx, `SELECT to_regclass('public.xic_batches')::text`).Scan(&batchTable); err != nil || batchTable == nil {
		logger.Fatal("batch schema missing — apply migration 009_xic_batch.sql", zap.Error(err))
	}

	var reconTable *string
	if err := dbpool.QueryRow(ctx, `SELECT to_regclass('public.xic_reconciliation_runs')::text`).Scan(&reconTable); err != nil || reconTable == nil {
		logger.Fatal("reconciliation schema missing — apply migration 010_xic_reconciliation.sql", zap.Error(err))
	}

	// counters from every engine feed /metrics; the hooks are set where each service is built
	metrics := signals.NewMetrics()

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
		Observe: metrics.Hook("inbound"),
	}
	inAdmin := handler.NewInboundAdmin(gw, inRepo, authzClient, logger)
	go gw.RunHandoff(evalCtx, time.Duration(cfg.XICHandoffIntervalSecs)*time.Second, 100, func(n int, err error) {
		if err != nil {
			logger.Warn("inbound handoff", zap.Int("due", n), zap.Error(err))
		}
	})

	// XIC-04 outbound engine (ZS-SVC-T-001 section 9): durable attempts, ambiguity-safe submission and resolution.
	// Destinations come from connections' governed endpoint profiles and are checked against egress policy before any
	// credential is fetched; credential material comes from the vault's use operation.
	obRepo := store.NewOutboundRepo(st)
	obSvc := &outbound.Service{
		Repo: obRepo, Conns: connSvc, Secrets: outbound.NewVaultSecrets(cfg.VaultServiceURL, "connectivity-api-bridge-svc"), Egress: egressPolicy(cfg, logger),
		Cfg: outbound.Config{BodyRetention: time.Duration(cfg.XICOutboundBodyRetentionHours) * time.Hour, BodyMaxBytes: cfg.XICOutboundBodyMaxBytes,
			BodyDenyClasses: cfg.XICOutboundBodyDenyClasses, RequestTimeout: time.Duration(cfg.XICOutboundRequestTimeoutSecs) * time.Second,
			SubmitLease: time.Duration(cfg.XICOutboundSubmitLeaseSecs) * time.Second, ResolveBackoff: time.Duration(cfg.XICOutboundResolveBackoffSecs) * time.Second,
			ResolveBackoffMax: 15 * time.Minute, MaxUnknownAge: time.Duration(cfg.XICOutboundMaxUnknownAgeHours) * time.Hour, MaxResponseBytes: 64 << 10,
			CallbackMaxBodyBytes: cfg.XICCallbackMaxBodyBytes, CallbackConnRatePerMin: cfg.XICCallbackConnRatePerMin, CallbackIPRatePerMin: cfg.XICCallbackIPRatePerMin,
			CallbackInvalidPerMin: cfg.XICCallbackInvalidPerMin},
		EdgeSecret: []byte(cfg.XICEdgeSecret), Observe: metrics.Hook("outbound"),
	}
	obHandler := handler.NewOutbound(obSvc, obRepo, authzClient, logger)
	go obSvc.RunResolver(evalCtx, time.Duration(cfg.XICOutboundResolverIntervalSecs)*time.Second, 100, func(n int, err error) {
		if err != nil {
			logger.Warn("outbound resolver", zap.Int("due", n), zap.Error(err))
		}
	})

	// XIC-05 batch and file exchange and replay (ZS-SVC-T-001 section 10): the HTTPS object channel, the SFTP pull
	// poller, the batch worker and the replay governance API. Every parsed byte passes a malware scanner; a deployment
	// with no scanner does not get a "clean" default, so the channel stays closed (fail closed).
	inRepoVersions := regRepo
	gw.Versions = inRepoVersions
	batchRepo := store.NewBatchRepo(st)
	var batchSvc *batch.Service
	if cfg.XICBatchEnabled || cfg.XICSFTPPollIntervalSecs > 0 {
		chain, scanErr := scan.Require(cfg.XICBatchClamAVAddr, cfg.XICBatchAllowBuiltinScanOnly, cfg.XICBatchMaxFileBytes, time.Duration(cfg.XICBatchScanTimeoutSecs)*time.Second)
		if scanErr != nil {
			logger.Error("batch intake is REFUSED: no malware scanner is configured (set XIC_BATCH_CLAMAV_ADDR; development may set XIC_BATCH_ALLOW_BUILTIN_SCAN_ONLY=true)")
		} else {
			batchSvc = &batch.Service{
				Repo: batchRepo, Conns: connSvc, In: gw, Secrets: inbound.NewVaultSecrets(cfg.VaultServiceURL, "connectivity-api-bridge-svc"), EdgeSecret: []byte(cfg.XICEdgeSecret), Scanner: chain,
				Observe: metrics.Hook("batch"),
				Cfg: batch.Config{MaxRequestBytes: cfg.XICBatchMaxRequestBytes, MaxFileBytes: cfg.XICBatchMaxFileBytes, MaxDecompressedBytes: cfg.XICBatchMaxDecompressedBytes,
					ContentRetention: time.Duration(cfg.XICBatchContentRetentionHrs) * time.Hour, ProcessingGrace: time.Duration(cfg.XICBatchProcessingGraceMins) * time.Minute,
					ContentDenyClasses: cfg.XICBatchContentDenyClasses, ConnRatePerMin: cfg.XICBatchConnRatePerMin, IPRatePerMin: cfg.XICBatchIPRatePerMin,
					ProcessLease: 2 * time.Minute, RowChunk: cfg.XICBatchRowChunk, RefreshPending: 30 * time.Second, RefreshPartial: 5 * time.Minute, ProcessBackoff: 30 * time.Second,
					ReplayMaxItems: cfg.XICReplayMaxItems, ReplayApprovalRows: cfg.XICReplayApprovalRows, ReplayWindowMax: time.Duration(cfg.XICReplayWindowMaxHours) * time.Hour,
					RegulatedClasses: cfg.XICReplayRegulatedClasses, ReplayLease: 5 * time.Minute},
			}
			go batchSvc.RunWorker(evalCtx, time.Duration(cfg.XICBatchWorkerIntervalSecs)*time.Second, 100, func(n int, err error) {
				if err != nil {
					logger.Warn("batch worker", zap.Int("due", n), zap.Error(err))
				}
			})
			if cfg.XICSFTPPollIntervalSecs > 0 {
				puller := &sftppull.Puller{Conns: connSvc, Source: connRepo, Batches: batchSvc, Secrets: inbound.NewVaultSecrets(cfg.VaultServiceURL, "connectivity-api-bridge-svc"),
					Egress: egressPolicy(cfg, logger), Cfg: sftppull.Config{Inbox: cfg.XICSFTPInbox, Done: cfg.XICSFTPDone, MaxFileBytes: cfg.XICBatchMaxFileBytes, Port: cfg.XICSFTPDefaultPort}}
				go puller.Run(evalCtx, time.Duration(cfg.XICSFTPPollIntervalSecs)*time.Second, func(n int, err error) {
					if err != nil {
						logger.Warn("sftp pull", zap.Int("taken", n), zap.Error(err))
					}
				})
				logger.Info("sftp pull poller enabled")
			}
		}
	}

	// XIC-05 reconciliation (ZS-SVC-T-001 section 10.4) and operational signals (section 17.1). The comparison reads the
	// real exchange, attempt, receipt and batch tables; a scheduled run needs no declaration because it reconciles what
	// the provider already declared (callbacks, manifests). Counters from every engine feed /metrics.
	sigRepo := store.NewSignalsRepo(st)
	reconRepo := store.NewReconRepo(st)
	policies := recon.DefaultPolicies()
	if cfg.XICReconPoliciesJSON != "" {
		var over map[string]recon.Policy
		if err := json.Unmarshal([]byte(cfg.XICReconPoliciesJSON), &over); err != nil {
			logger.Fatal("XIC_RECON_POLICIES_JSON is invalid", zap.Error(err))
		}
		for k, v := range over {
			v.Domain = k
			policies[k] = v
		}
	}
	reconSvc := &recon.Service{Repo: reconRepo, Conns: connSvc, Connectors: regRepo, Live: connRepo, Policies: policies,
		Cfg:     recon.Config{MaxPeriod: time.Duration(cfg.XICReconMaxPeriodHours) * time.Hour, Lease: 5 * time.Minute, ScheduleEvery: time.Duration(cfg.XICReconScheduleEverySecs) * time.Second, MaxExternalRef: 10000},
		Observe: metrics.Hook("reconciliation")}
	go reconSvc.RunWorker(evalCtx, time.Duration(cfg.XICReconWorkerIntervalSecs)*time.Second, 100, func(n int, err error) {
		if err != nil {
			logger.Warn("reconciliation worker", zap.Int("due", n), zap.Error(err))
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
	handler.RegisterOutboundRoutes(r, obHandler)
	handler.RegisterCallbackAdminRoutes(r, handler.NewCallbackAdmin(obRepo, authzClient, logger))
	handler.RegisterReconRoutes(r, handler.NewRecon(reconSvc, reconRepo, sigRepo, authzClient, logger))
	if batchSvc != nil {
		handler.RegisterBatchAdminRoutes(r, handler.NewBatchAdmin(batchSvc, batchRepo, authzClient, logger))
	}

	root := chi.NewRouter()
	root.Use(chimiddleware.RequestID)
	root.Use(chimiddleware.Logger)
	root.Use(chimiddleware.Recoverer)
	if cfg.XICInboundEnabled {
		root.Mount("/v1/inbound", handler.NewIngest(gw, cfg.XICInboundMaxBodyBytes, cfg.XICInboundTrustXFF, logger).Router())
		logger.Info("inbound ingest endpoint enabled")
	}
	if cfg.XICCallbacksEnabled {
		root.Mount("/v1/callbacks", handler.NewCallback(obSvc, cfg.XICCallbackMaxBodyBytes, cfg.XICInboundTrustXFF, logger).Router())
		logger.Info("provider callback endpoint enabled")
	}
	if cfg.XICBatchEnabled && batchSvc != nil {
		root.Mount("/v1/batch-intake", handler.NewBatchIntake(batchSvc, cfg.XICBatchMaxRequestBytes, cfg.XICInboundTrustXFF, logger).Router())
		logger.Info("batch intake endpoint enabled")
	}
	if cfg.XICMetricsToken != "" {
		root.Method("GET", "/metrics", handler.NewMetrics(metrics, sigRepo, cfg.XICMetricsToken, time.Duration(cfg.XICMetricsWindowHours)*time.Hour, logger))
		logger.Info("metrics endpoint enabled")
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
