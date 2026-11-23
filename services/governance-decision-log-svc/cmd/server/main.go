// Package main is the entry point for governance-decision-log-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore
//  5. Construct event publisher (real Kafka producer)
//  6. Construct HTTP handler + mount routes on chi router
//  7. Mount health probes (/healthz, /readyz)
//  8. Start HTTP server with graceful shutdown
package main

import (
	"context"
	"errors"
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
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/governance-decision-log-svc/internal/authz"
	"zoiko.io/governance-decision-log-svc/internal/config"
	svcenvelope "zoiko.io/governance-decision-log-svc/internal/envelope"
	"zoiko.io/governance-decision-log-svc/internal/handler"
	"zoiko.io/governance-decision-log-svc/internal/health"
	"zoiko.io/governance-decision-log-svc/internal/mtls"
	"zoiko.io/governance-decision-log-svc/internal/outbox"
	"zoiko.io/governance-decision-log-svc/internal/policyclient"
	"zoiko.io/governance-decision-log-svc/internal/sealer"
	"zoiko.io/governance-decision-log-svc/internal/store"
	"zoiko.io/governance-decision-log-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented to mtls-management-svc
// when provisioning this service's own client-side mTLS identity — the same
// platform-wide scope tax-rules-svc's mTLS pilot uses.
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

	log.Info("governance-decision-log-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "governance-decision-log-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("governance-decision-log-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as the other Tier 0 services.
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

	// Verify connectivity at startup — fail fast rather than silently degrade.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4b. Kafka producer & outbox relay ───────────────────────────────────────
	// This service is the governance evidence ledger (Tier 0). A deployment
	// with no broker would accept decisions whose events never leave the
	// outbox, so that is refused outside local development (GCP §1: evidence
	// is not an afterthought). Locally, KAFKA_BROKERS= runs without a relay and
	// events wait in the outbox until one runs.
	if len(cfg.Kafka.Brokers) == 0 {
		if strings.EqualFold(cfg.Env, "production") || strings.EqualFold(cfg.Env, "staging") {
			log.Fatal("KAFKA_BROKERS is empty: refusing to start the governance ledger without an event backbone",
				zap.String("env", cfg.Env))
		}
		log.Warn("KAFKA_BROKERS is empty: outbox relay disabled; events accumulate in the outbox")
	} else {
		kafkaWriter := &kafka.Writer{
			Addr:         kafka.TCP(cfg.Kafka.Brokers...),
			Topic:        cfg.Kafka.Topic,
			Balancer:     &kafka.Hash{},
			BatchTimeout: 10 * time.Millisecond,
			RequiredAcks: kafka.RequireAll,
			// Without this, a broker that has not yet had the topic created
			// answers every publish with Unknown Topic Or Partition and the
			// relay retries forever. Same setting as the other services.
			AllowAutoTopicCreation: true,
		}
		defer func() {
			if err := kafkaWriter.Close(); err != nil {
				log.Error("kafka writer close failed", zap.Error(err))
			}
		}()

		outboxWorker := outbox.NewWorker(pool, kafkaWriter, log,
			outbox.WithObserver(metrics), outbox.WithRetention(cfg.OutboxRetention))
		outboxWorker.Start(context.Background())
		defer outboxWorker.Stop()
	}

	// ── 5. Store ──────────────────────────────────────────────────────────────
	pgStore := store.New(pool, log)

	// Event publishing uses transactional outbox (see 4b above)

	// Background work (certificate renewal, CRL refresh, scheduled sealing)
	// stops with this context at shutdown.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	// Workload identity (X5): one certificate from mtls-management-svc, used
	// for the inbound mTLS listener and for calls to authorization-svc. It is
	// renewed at two thirds of its lifetime; both halves read it per handshake.
	var workloadID *mtls.RotatingIdentity
	if cfg.MTLSEnabled || cfg.AuthzMTLSEnabled {
		provision := func(ctx context.Context) (*mtls.Identity, error) {
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return mtls.Provision(pctx, cfg.MTLSManagementServiceURL, "governance-decision-log-svc", platformScopeID,
				mtls.LoadBootstrapToken(cfg.MTLSBootstrapTokenPath))
		}
		first, err := provision(context.Background())
		if err != nil {
			log.Fatal("mtls: failed to provision this service's workload identity", zap.Error(err))
		}
		workloadID, err = mtls.NewRotatingIdentity(first)
		if err != nil {
			log.Fatal("mtls: workload identity unusable", zap.Error(err))
		}
		go workloadID.Run(bgCtx, provision, log)
		log.Info("mtls: workload identity provisioned", zap.Bool("listener", cfg.MTLSEnabled), zap.Bool("authz_client", cfg.AuthzMTLSEnabled),
			zap.Time("not_after", workloadID.NotAfter()), zap.Time("renew_at", workloadID.RenewAt()))
	}

	// AuthZ client. Refuses to start in production/staging against a
	// placeholder URL — no service may silently fall back to permit-all.
	var authzClient authz.Client
	if cfg.AuthzMTLSEnabled {
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, log, workloadID.HTTPClient()).WithWorkload("governance-decision-log-svc")
	} else {
		authzClient, err = authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
		if err != nil {
			log.Fatal("authz client construction failed", zap.Error(err))
		}
	}

	// Identity gate (Governance audit A1 / D-05, X5). X-Principal-Id and
	// X-Tenant-Id were trusted as sent and port 8083 is published, so anyone who
	// could reach it could write decisions as any principal. The gate verifies
	// identity-context-svc's signed envelope (JWKS) and that its session is still
	// live (AU-01), and makes the verified claims the only source of those
	// headers. IDENTITY_VERIFY: require (default outside local) | warn.
	// Peers: the workloads that may vouch for X-Principal-Id over verified mTLS
	// — the two ledger writers whose relays act for no signed-in person.
	// IDENTITY_PEERS overrides (e.g. to add the readers contract-lifecycle-svc,
	// decision-support-svc and evidence-manifest-svc once they call over mTLS).
	gate, gateSettings, err := idenv.NewGateFromEnv("governance-decision-log-svc", log,
		idenv.EnvOptions{DefaultPeers: []string{"policy-svc", "workflow-svc"}})
	if err != nil {
		log.Fatal("identity gate configuration refused", zap.Error(err))
	}
	log.Info("identity gate", gateSettings.Fields()...)

	// ── 6. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("governance-decision-log-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(gate.Middleware)
	r.Use(correlationIDMiddleware)
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	// Refusals rendered as RFC 9457 problem+json, like every other error here.
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	policyClient := policyclient.NewHTTPClient(cfg.PolicyServiceURL)
	h := handler.New(pgStore, authzClient, policyClient, cfg.AuthZPlatformScopeID, log)
	if strings.EqualFold(cfg.ReadAuthzMode, "warn") {
		log.Warn("DECISION_LOG_READ_AUTHZ=warn: unauthorized reads are logged and served, not refused")
		h.WarnOnlyReadAuthz()
	}
	handler.RegisterRoutes(r, h)

	// Scheduled sealing (GOV-07): every tenant's unsealed decisions are sealed
	// into an evidence package each interval, after re-verifying the chain.
	if cfg.EvidenceSealInterval > 0 {
		go sealer.New(pgStore, cfg.EvidenceSealPrincipalID, log, metrics).Run(bgCtx, cfg.EvidenceSealInterval)
		log.Info("scheduled evidence sealing on", zap.Duration("interval", cfg.EvidenceSealInterval),
			zap.String("sealed_by", cfg.EvidenceSealPrincipalID))
	} else {
		log.Warn("EVIDENCE_SEAL_INTERVAL=0: scheduled sealing off; packages are sealed only on request")
	}

	// ── 7. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 8. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + strconv.Itoa(cfg.Port)
	// ReadHeaderTimeout is the one that is easy to miss, and the reason all four
	// are stated together. ReadTimeout bounds a whole request, so a client that
	// dribbles a BODY is already cut off -- but a connection that sends a partial
	// HEADER and then stalls holds a goroutine and a descriptor for that entire
	// window without ever becoming a request. Enough of those exhaust the process
	// while every metric still reads healthy.
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// The mTLS listener serves the same router: what changes is only that
	// every connection presents a verified platform certificate, which the
	// identity gate reads as the calling workload.
	var mtlsSrv *http.Server
	if cfg.MTLSEnabled {
		// Revocation: a peer certificate on mtls-management-svc's CRL is
		// refused at the handshake, and so is every client certificate while
		// no current CRL is held (fail closed).
		if cfg.MTLSRevocationCheck != "off" {
			revocation := mtls.NewRevocationChecker(cfg.MTLSCRLURL, workloadID.CACertificates)
			if err := revocation.Refresh(context.Background()); err != nil {
				log.Warn("mtls: initial CRL fetch failed — :8450 refuses client certificates until one is fetched", zap.Error(err))
			}
			workloadID.SetPeerVerifier(revocation.VerifyConnection)
			go revocation.Run(bgCtx, cfg.MTLSCRLRefresh, log)
			log.Info("mtls: client certificate revocation check on", zap.String("crl_url", cfg.MTLSCRLURL), zap.Duration("refresh", cfg.MTLSCRLRefresh))
		} else {
			log.Warn("mtls: MTLS_REVOCATION_CHECK=off — a revoked peer certificate is admitted until it expires (ENV=local only)")
		}
		mtlsSrv = &http.Server{
			Addr:              ":" + strconv.Itoa(cfg.MTLSPort),
			Handler:           r,
			TLSConfig:         workloadID.ServerTLSConfig(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			log.Info("mTLS server listening", zap.String("addr", mtlsSrv.Addr))
			if err := mtlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()
	}

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
	if mtlsSrv != nil {
		if err := mtlsSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("mTLS listener shutdown failed", zap.Error(err))
		}
	}
	log.Info("server stopped")
}

// correlationIDMiddleware echoes the caller's X-Correlation-ID. It used to
// invent one from the request id before the input contract could refuse a
// request without one: correlation_id is mandatory end to end (§4), and a
// made-up one traces nothing.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}
