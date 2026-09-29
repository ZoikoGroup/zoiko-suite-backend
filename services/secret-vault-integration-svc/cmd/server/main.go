// Package main is the entry point for secret-vault-integration-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore
//  5. Construct the vault backend (LocalFileVaultBackend for v1 — see
//     internal/vault/backend.go and context.md §7.6)
//  6. Construct event publisher over a kafka.Writer (a logged no-op when no
//     brokers are configured, which is refused outside local development)
//  7. Construct HTTP handler + mount routes on chi router
//  8. Mount health probes (/healthz, /readyz)
//  9. Start HTTP server with graceful shutdown
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"go.uber.org/zap"

	"github.com/segmentio/kafka-go"

	"zoiko.io/secret-vault-integration-svc/internal/authz"
	"zoiko.io/secret-vault-integration-svc/internal/config"
	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"
	"zoiko.io/secret-vault-integration-svc/internal/events"
	"zoiko.io/secret-vault-integration-svc/internal/handler"
	"zoiko.io/secret-vault-integration-svc/internal/health"
	"zoiko.io/secret-vault-integration-svc/internal/inboundmtls"
	svcmiddleware "zoiko.io/secret-vault-integration-svc/internal/middleware"
	"zoiko.io/secret-vault-integration-svc/internal/mtls"
	"zoiko.io/secret-vault-integration-svc/internal/rotation"
	"zoiko.io/secret-vault-integration-svc/internal/store"
	"zoiko.io/secret-vault-integration-svc/internal/telemetry"
	"zoiko.io/secret-vault-integration-svc/internal/vault"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	// ── 2. Logger ─────────────────────────────────────────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("secret-vault-integration-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "secret-vault-integration-svc", cfg.OTELExporterEndpoint)
	if err != nil {
		log.Fatal("otel tracing init failed", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Error("otel tracer provider shutdown failed", zap.Error(err))
		}
	}()

	metrics := telemetry.NewMetrics("secret-vault-integration-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		log.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatal("failed to create db pool", zap.Error(err))
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Store ──────────────────────────────────────────────────────────────
	pgStore := store.New(pool, log)

	// ── 5. Vault backend ──────────────────────────────────────────────────────
	// Master key isolation (SEC-INV-07). VAULT_KEK_PROVIDER=transit|gcpkms
	// keeps the master key in a KMS and envelope-encrypts material under
	// per-secret data keys; this process never holds the key that protects
	// the store. VAULT_KEK_PROVIDER=local (the in-process key file / hex) is
	// local dev only and refused in staging/production by ValidateVault.
	if err := cfg.ValidateVault(); err != nil {
		log.Fatal("vault backend refused", zap.Error(err))
	}
	if err := cfg.ValidateLeaseCeiling(); err != nil {
		log.Fatal("lease ceiling refused", zap.Error(err))
	}
	var vaultBackend handler.VaultBackend
	switch cfg.VaultKEKProvider {
	case "transit", "gcpkms":
		var kek vault.KeyWrapper
		if cfg.VaultKEKProvider == "transit" {
			tw := &vault.TransitKeyWrapper{Addr: cfg.VaultTransitAddr, Mount: cfg.VaultTransitMount, KeyName: cfg.VaultTransitKey, TokenFile: cfg.VaultTransitTokenFile}
			// §9 non-exportable credentials: a master key Vault would export
			// is not isolated. Fatal where production secrets live.
			if verr := tw.VerifyNonExportable(context.Background()); verr != nil {
				if isRestrictedEnvironment(cfg.Env) {
					log.Fatal("transit master key refused", zap.Error(verr))
				}
				log.Warn("transit master key policy check failed (tolerated outside staging/production)", zap.Error(verr))
			}
			kek = tw
		} else {
			kek = &vault.GCPKMSKeyWrapper{KeyName: cfg.GCPKMSKeyName}
		}
		if cfg.VaultMigrateLegacyKeyFile != "" {
			n, merr := vault.MigrateLocalStore(context.Background(), cfg.VaultKeyPath, cfg.VaultMigrateLegacyKeyFile, kek)
			if merr != nil {
				log.Fatal("legacy vault store migration failed (legacy file restored)", zap.Error(merr))
			}
			log.Warn("legacy vault store migrated to KMS envelope encryption; delete the .legacy-bak file and the legacy key, then unset VAULT_MIGRATE_LEGACY_KEY_FILE",
				zap.Int("records", n), zap.String("kek", kek.KeyRef()))
		}
		envBackend, eerr := vault.NewEnvelopeFileVaultBackend(context.Background(), cfg.VaultKeyPath, kek)
		if eerr != nil {
			log.Fatal("failed to construct KMS-backed vault backend", zap.Error(eerr))
		}
		log.Info("vault backend: KMS envelope encryption", zap.String("kek", kek.KeyRef()))
		vaultBackend = envBackend
	default:
		if isRestrictedEnvironment(cfg.Env) {
			log.Warn("VAULT_KEK_PROVIDER=local in " + cfg.Env + " under ALLOW_IN_PROCESS_MASTER_KEY: the master key is held in this process (SEC-INV-07 exception)")
		}
		var local *vault.LocalFileVaultBackend
		switch {
		case cfg.VaultMasterKeyFile != "":
			local, err = vault.NewLocalFileVaultBackendFromFile(cfg.VaultKeyPath, cfg.VaultMasterKeyFile)
		case cfg.VaultMasterKeyHex != "":
			if isRestrictedEnvironment(cfg.Env) && !cfg.AllowPlaintextMasterKey {
				log.Fatal("refusing to run with VAULT_MASTER_KEY_HEX in " + cfg.Env + ": use VAULT_MASTER_KEY_FILE (0600 owner-only) or set ALLOW_PLAINTEXT_MASTER_KEY=true to override")
			}
			if !isRestrictedEnvironment(cfg.Env) {
				log.Warn("VAULT_MASTER_KEY_HEX is deprecated — prefer VAULT_MASTER_KEY_FILE (owner-only key file); raw env key is tolerated only outside staging/production")
			}
			local, err = vault.NewLocalFileVaultBackend(cfg.VaultKeyPath, cfg.VaultMasterKeyHex)
		default:
			log.Fatal("no vault master key configured: set VAULT_KEK_PROVIDER=transit|gcpkms, or VAULT_MASTER_KEY_FILE for local dev")
		}
		if err != nil {
			log.Fatal("failed to construct vault backend", zap.Error(err))
		}
		vaultBackend = local
	}

	// ── 6. Event publisher ────────────────────────────────────────────────────
	kafkaWriter := newKafkaWriter(cfg, log)
	if kafkaWriter != nil {
		defer func() { _ = kafkaWriter.Close() }()
	}
	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)

	// AuthZ client. Refuses to start in production/staging against a
	// placeholder URL — no service may silently fall back to permit-all.
	var authzClient authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "secret-vault-integration-svc", platformScopeID, cfg.MTLSBootstrapTokenPath)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewHTTPClientWithHTTPClient(cfg.AuthzMTLSURL, log, mtlsHTTPClient)
	} else {
		authzClient, err = authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
		if err != nil {
			log.Fatal("authz client construction failed", zap.Error(err))
		}
	}

	// ── 7. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("secret-vault-integration-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	// The caller's tenant scope, from the header the gateway sets. This service
	// used to read no such header at all: every tenant-scoped decision came from
	// a query parameter or a request body.
	r.Use(svcmiddleware.TenantContext())
	// Inbound client-certificate identity revalidation (Gap 2). When
	// TLS_CLIENT_CA_FILE is set (MTLS_IDENTITY_CHECK defaults on with it), a
	// request whose presented certificate does not name every X-Principal-Id /
	// X-Workload-Id it carries is refused before any business logic runs —
	// unless the certificate is a trusted forwarder (MTLS_TRUSTED_FORWARDERS).
	r.Use(inboundmtls.IdentityCheck(cfg.MTLSIdentityCheck, cfg.MTLSTrustedForwarders, log))
	r.Use(inboundmtls.CertPolicy(cfg.MTLSMaxClientCertLifetime, cfg.MTLSRequiredURIPrefix, log))
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, vaultBackend, publisher, authzClient, cfg.AuthZPlatformScopeID, cfg.MaxLeaseDurationSeconds, log).UseMetrics(metrics).RequireSharedSecretException(cfg.RequireSharedSecretException)
	handler.RegisterRoutes(r, h)

	// ── 7b. Automated rotation sweeper (Gap 5a) ───────────────────────────────
	// ROTATION_SWEEP_INTERVAL > 0 enables it. The sweeper is an unattended,
	// platform-scoped operator: it reuses the exact rotate-and-mass-revoke
	// core PerformRotation that the HTTP rotate endpoint runs, so its audit
	// trail and lease invalidation are identical, not a parallel copy.
	if cfg.RotationSweepInterval > 0 {
		sweeper := rotation.New(pgStore, h, log.With(zap.String("component", "rotation-sweeper")), cfg.RotationSweepInterval, cfg.RotationSweepActor)
		sweepCtx, sweepCancel := context.WithCancel(context.Background())
		defer sweepCancel()
		go func() {
			if err := sweeper.Run(sweepCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("rotation sweeper stopped", zap.Error(err))
			}
		}()
		log.Info("automated rotation sweeper enabled",
			zap.Duration("interval", cfg.RotationSweepInterval),
			zap.String("actor", cfg.RotationSweepActor),
		)
	}

	// ── 8. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 9. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + strconv.Itoa(cfg.Port)

	// Inbound TLS gate (Gap 2). Set TLS_CERT_FILE + TLS_KEY_FILE to serve
	// HTTPS; TLS_CLIENT_CA_FILE additionally requires client certificates
	// (mTLS). In staging/production, anything short of mTLS with the identity
	// check on is refused unless ALLOW_INSECURE_INBOUND=true — server-only TLS
	// used to pass, which encrypts but authenticates no caller. The CA/cert
	// provisioning itself (what issues those files, and what terminates TLS
	// at the gateway) is the documented CROSS-SERVICE/infra requirement.
	if err := cfg.ValidateInbound(); err != nil {
		log.Fatal("inbound transport refused", zap.Error(err))
	}
	tlsConfig, err := inboundTLSConfig(cfg, log)
	if err != nil {
		log.Fatal("failed to construct inbound TLS config", zap.Error(err))
	}

	// ReadHeaderTimeout is the one that is easy to miss, and the reason all four
	// are stated together. ReadTimeout bounds a whole request, so a client that
	// dribbles a BODY is already cut off -- but a connection that sends a partial
	// HEADER and then stalls holds a goroutine and a descriptor for that entire
	// window without ever becoming a request. Enough of those exhaust the process
	// while every metric still reads healthy.
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
			log.Info("HTTPS server listening (mutual TLS enabled)", zap.String("addr", addr), zap.Bool("mtls", tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert))
			if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
			return
		}
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		log.Fatal("server error", zap.Error(err))
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("server stopped")
}

// correlationIDMiddleware propagates X-Correlation-ID through every request.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}

// isRestrictedEnvironment reports whether the deployment environment is one
// where the insecure fallbacks (plaintext master key, unauthenticated
// inbound) are refused by default.
func isRestrictedEnvironment(env string) bool {
	return config.IsRestrictedEnvironment(env)
}

// inboundTLSConfig builds the inbound TLS server config, or nil when no
// TLS_CERT_FILE/TLS_KEY_FILE is configured (plain HTTP, gate enforced by
// the caller). When TLS_CLIENT_CA_FILE is also set, the server requires
// client certificates (mTLS).
func inboundTLSConfig(cfg *config.Config, log *zap.Logger) (*tls.Config, error) {
	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		return nil, nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must both be set to enable inbound TLS")
	}
	return inboundmtls.NewServerTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile, log)
}

// newKafkaWriter builds the event-backbone producer, or nil when no brokers
// are configured.
//
// Kafka connects lazily on first write, so it is not a fail-fast startup
// dependency like Postgres — the same posture as obligations-svc and
// jurisdiction-rules-svc. A nil writer makes every publish a logged no-op,
// which keeps a single-service local run to two containers; that fallback is
// refused outside local development, because a production deployment
// silently publishing nothing is exactly the failure events exist to prevent.
func newKafkaWriter(cfg *config.Config, log *zap.Logger) *kafka.Writer {
	if len(cfg.Kafka.Brokers) == 0 {
		if strings.EqualFold(cfg.Env, "production") || strings.EqualFold(cfg.Env, "staging") {
			log.Fatal("KAFKA_BROKERS must be set in " + cfg.Env + " environment")
		}
		log.Warn("no Kafka brokers configured — domain events will be dropped")
		return nil
	}
	return &kafka.Writer{
		Addr:     kafka.TCP(cfg.Kafka.Brokers...),
		Topic:    cfg.Kafka.Topic,
		Balancer: &kafka.LeastBytes{},
		// Required even though the broker sets auto.create.topics.enable:
		// kafka-go defaults this to false and never asks the broker to
		// auto-create in its metadata request, so a write to a topic that does
		// not exist yet fails with "Unknown Topic Or Partition" regardless of
		// the broker-side setting. Matches the platform-wide fix in 7589bc3.
		AllowAutoTopicCreation: true,
		// Bounded so an unreachable broker delays a response rather than
		// holding the request open — the write is already committed by the
		// time an event is emitted.
		WriteTimeout: 5 * time.Second,
		// Without this, every write to this service costs an extra second.
		// kafka-go batches, and BatchTimeout defaults to 1s: a synchronous
		// WriteMessages of a single message waits for the batch to fill (100
		// messages) or for that timer, whichever comes first. These events are
		// emitted one per state transition, so the batch never fills and the
		// timer always wins — and publishing is on the request path, so the
		// caller pays for it. Ordering and synchronous delivery are unchanged;
		// only the artificial wait goes away.
		BatchTimeout: 10 * time.Millisecond,
	}
}
