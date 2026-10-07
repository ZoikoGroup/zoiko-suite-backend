// Package main is the entry point for authorization-svc.
//
// Wiring order:
//  1. Load config from environment
//  2. Initialise structured logger (zap)
//  3. Connect to PostgreSQL pool (pgxpool) — Tier 0 pool sizing
//  4. Construct PgStore, Kafka producer, jurisdiction-rules-svc validator
//  5. Construct HTTP handler + mount routes on chi router
//  6. Mount health probes (/healthz, /readyz)
//  7. Start HTTP server with graceful shutdown
package main

import (
	"context"
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
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/cache"
	"zoiko.io/authorization-svc/internal/config"
	svcenvelope "zoiko.io/authorization-svc/internal/envelope"
	"zoiko.io/authorization-svc/internal/events"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/health"
	"zoiko.io/authorization-svc/internal/jurisdiction"
	"zoiko.io/authorization-svc/internal/mtls"
	"zoiko.io/authorization-svc/internal/retention"
	"zoiko.io/authorization-svc/internal/siem"
	"zoiko.io/authorization-svc/internal/store"
	"zoiko.io/authorization-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented when provisioning this
// service's own mTLS identity — an infrastructure certificate, not scoped
// to any one tenant's data, same convention as AUTHZ_PLATFORM_SCOPE_ID
// elsewhere in this codebase.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

// partitionWatchInterval is how often each consumer group re-reads partition
// metadata and rejoins if it changed.
//
// 30s rather than kafka-go's 5s default. The condition it recovers from — a
// join assigned no partitions — is rare and not time-critical to within
// seconds, and this poll is a metadata round-trip per interval per reader
// against a Tier-0 broker. 30s bounds a silent stall at half a minute while
// costing two requests a minute.
const partitionWatchInterval = 30 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("authorization-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("jurisdiction_rules_url", cfg.JurisdictionRulesURL),
	)

	shutdownTracing, err := telemetry.InitTracing(context.Background(), "authorization-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("authorization-svc")

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

	pgStore := store.New(pool, log)

	// The evaluation reads on the /v1/authorize path go through a short-lived
	// in-process cache. It invalidates itself on every write that passes
	// through it, so a grant changed via this service's own admin API takes
	// effect on the next request; the TTL is what bounds a change made
	// elsewhere. It does NOT cache the decision or the decision-log insert —
	// see internal/cache's package comment, which is the part worth reading
	// before changing this line.
	authzStore := cache.New(pgStore, cfg.CacheTTL, log)
	if authzStore.Enabled() {
		log.Info("authorization read cache enabled", zap.Duration("ttl", cfg.CacheTTL))
	} else {
		log.Info("authorization read cache DISABLED — every evaluation reads the database")
	}

	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	jurisdictionValidator := jurisdiction.NewHTTPValidator(cfg.JurisdictionRulesURL, log)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("authorization-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// handler.EnvelopePolicy, not svcenvelope.ServicePolicy: the generated
	// policy classifies POST /v1/authorize as a material write, which refused
	// almost every caller of the evaluation endpoint. See handler.MaterialWrite.
	r.Use(svcenvelope.Middleware(handler.EnvelopePolicy(), svcenvelope.DefaultReporter()))

	siemClient := siem.New(cfg.SIEMServiceURL, "authorization-svc", log)
	// Drains the SIEM queue on shutdown. Streaming is fire-and-forget, so
	// without this a SIGTERM would discard security events already accepted
	// from a request that has long since been answered.
	defer siemClient.Close()
	h := handler.New(authzStore, publisher, jurisdictionValidator, siemClient, cfg.PlatformScopeEntityID, log)
	handler.RegisterRoutes(r, h)

	if cfg.PlatformScopeEntityID == "" {
		// Not fatal, but worth one loud line at boot: without it every
		// platform-wide act (authoring a platform-wide SoD or ABAC rule) is
		// refused, and every /v1/authorize call naming
		// handler.PlatformScopeSentinel answers 400. That is the correct
		// fail-closed default, and it is also invisible from the outside.
		log.Warn("AUTHZ_PLATFORM_SCOPE_ENTITY_ID is unset — platform-wide rule authoring and legal_entity_id=PLATFORM requests will be refused")
	}

	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	addr := ":" + strconv.Itoa(cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// mTLS pilot (material-path only, docs/original_doc/zoiko_suite_doc5.txt:
	// 76,251): a SECOND listener, same router, gated behind MTLS_ENABLED so
	// every caller still on the plain port above keeps working unchanged.
	var mtlsSrv *http.Server
	if cfg.MTLSEnabled {
		bootstrapToken := loadMTLSBootstrapToken(cfg.MTLSBootstrapTokenPath, log)
		identity, err := mtls.ProvisionServerIdentity(context.Background(), cfg.MTLSManagementServiceURL, "authorization-svc", platformScopeID, bootstrapToken)
		if err != nil {
			log.Fatal("mtls: failed to provision server identity", zap.Error(err))
		}
		tlsConfig, err := mtls.ServerTLSConfig(identity)
		if err != nil {
			log.Fatal("mtls: failed to build server tls config", zap.Error(err))
		}
		mtlsAddr := ":" + strconv.Itoa(cfg.MTLSPort)
		mtlsSrv = &http.Server{
			Addr:         mtlsAddr,
			Handler:      r,
			TLSConfig:    tlsConfig,
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 15 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
		go func() {
			log.Info("mTLS listener starting", zap.String("addr", mtlsAddr))
			if err := mtlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()
	}

	// ── delegation projection consumer ──────────────────────────────────────
	//
	// Consumes delegated-authority-svc's authority.delegated /
	// authority.revoked / authority.expired into this service's own
	// delegated_authorities table, so /v1/authorize's delegation layer
	// resolves the grants the authoritative service actually made. See
	// internal/events.Consumer for why these three events and not the other
	// four v1 declined to consume.
	//
	// Started in a goroutine and never allowed to be fatal: this is the
	// platform's authorization engine and it must answer whether or not a
	// broker is reachable. An unreachable broker means the projection goes
	// stale, which the consumer logs; it does not mean requests stop being
	// evaluated.
	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	defer stopConsumer()
	if cfg.Kafka.DelegationTopic == "" {
		log.Info("delegation projection disabled (KAFKA_DELEGATION_TOPIC empty) — delegated_authorities is fed only by this service's own admin API")
	} else {
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: cfg.Kafka.Brokers,
			GroupID: cfg.Kafka.DelegationGroupID,
			Topic:   cfg.Kafka.DelegationTopic,
			// A consumer group commits offsets, so a restart resumes where it
			// stopped rather than replaying the whole topic — and the writes
			// are idempotent either way (upsert on the upstream delegation
			// id), so a replay converges instead of duplicating grants.
			MinBytes: 1,
			MaxBytes: 10e6,
			MaxWait:  500 * time.Millisecond,
			// See kafkaErrorLogger: without this, a consumer group that joins
			// and is assigned nothing does so in complete silence.
			ErrorLogger: kafkaErrorLogger(log, cfg.Kafka.DelegationTopic),
			// See the comment on the lifecycle reader below. Same hazard,
			// same recovery: a first join that is assigned no partitions
			// otherwise stays that way for the life of the process.
			WatchPartitionChanges:  true,
			PartitionWatchInterval: partitionWatchInterval,
		})
		// authzStore, not pgStore: a projected delegation must invalidate the
		// cached delegation lookups, or an upstream revocation would keep
		// granting for up to a TTL after it was applied.
		consumer := events.NewConsumer(log, authzStore)
		go consumer.Run(consumerCtx, reader)
	}

	// ── lifecycle consumer ──────────────────────────────────────────────────
	//
	// The other three consumed events Doc 03 §8.3 names, which four passes
	// recorded as having no producer. They do — under concrete names, on three
	// topics; see internal/events.LifecycleConsumer for the mapping and for
	// the one candidate that genuinely is not consumable.
	//
	// Two distinct jobs, one subscription:
	//
	//   principal.status.changed  is PROJECTED into
	//                             principal_status_projection and read as
	//                             layer 0 of /v1/authorize — a principal
	//                             identity-context-svc has suspended is denied
	//                             every action. Session eviction does not
	//                             cover this: 111 services call this endpoint
	//                             east-west on envelopes resolved before the
	//                             suspension.
	//   the rest                  INVALIDATE this replica's cached grant
	//                             reads. Admin writes reach ONE replica and
	//                             are announced to all of them, so consuming
	//                             the announcement is what stops the cache TTL
	//                             being the only bound on cross-replica
	//                             staleness.
	//
	// authzStore, not pgStore, and for both jobs: the projection has to
	// invalidate the cached layer-0 read or a suspension would take up to a
	// TTL to bite, and the invalidation hook only exists on the cache.
	//
	// Same posture as the delegation consumer on failure: never fatal, retried
	// forever, and a broker that is down means these two mechanisms degrade to
	// where they were before this consumer existed rather than stopping the
	// service.
	if len(cfg.Kafka.LifecycleTopics) == 0 {
		log.Info("lifecycle consumer disabled (KAFKA_LIFECYCLE_TOPICS empty) — principal suspensions will not be projected (layer 0 stays inert) and cross-replica cache invalidation falls back to the TTL")
	} else {
		lifecycleReader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: cfg.Kafka.Brokers,
			GroupID: cfg.Kafka.LifecycleGroupID,
			// GroupTopics, not Topic: one logical subscription over three
			// topics, one offset stream per topic managed by the group. Topic
			// and GroupTopics are mutually exclusive in kafka-go.
			GroupTopics: cfg.Kafka.LifecycleTopics,
			MinBytes:    1,
			MaxBytes:    10e6,
			MaxWait:     500 * time.Millisecond,
			ErrorLogger: kafkaErrorLogger(log, strings.Join(cfg.Kafka.LifecycleTopics, ",")),

			// ── recovery from a bad first join ──────────────────────────────
			//
			// MEASURED, not precautionary. On the first run of this consumer
			// the group joined, reported Stable with one member and **zero
			// partitions assigned**, and consumed nothing — while the service
			// log said only "lifecycle consumer started". A second run under a
			// different group id was assigned all three partitions
			// immediately, from the same image against the same broker, which
			// is what identifies this as a join-time race rather than a
			// configuration error.
			//
			// kafka-go defaults WatchPartitionChanges to FALSE, so a member
			// that is assigned nothing never re-checks: it heartbeats happily,
			// the group stays Stable, ReadMessage blocks forever, and the
			// broker reports a healthy group with a member holding no
			// partitions. Nothing in the Run loop can detect it — that loop
			// only sees errors RETURNED from ReadMessage, and this failure
			// returns nothing at all.
			//
			// On this service that failure mode means principal suspensions
			// silently never take effect, which is the one thing layer 0
			// exists to prevent. With the watch on, the reader re-reads
			// partition metadata every interval and rejoins when it differs,
			// so the bad assignment is corrected within one interval instead
			// of persisting until somebody restarts the pod.
			WatchPartitionChanges:  true,
			PartitionWatchInterval: partitionWatchInterval,
		})
		lifecycleConsumer := events.NewLifecycleConsumer(log, authzStore)
		go lifecycleConsumer.Run(consumerCtx, lifecycleReader)
		log.Info("lifecycle consumer wired", zap.Strings("topics", cfg.Kafka.LifecycleTopics))
	}

	// ── access_decision_log retention ──────────────────────────────────────
	//
	// One row per evaluation on a table that is correctly append-only, which
	// together describe unbounded growth. 000009 partitioned it by month and
	// wrote both maintenance helpers; nothing called either until this
	// sweeper existed, so the runway was whatever the migration created and
	// the retention window was configuration with no effect.
	//
	// pgStore, NOT authzStore: these are catalogue-level maintenance calls on
	// the parent table, and routing them through the evaluation cache would
	// invalidate every namespace on a sweep for no reason — nothing the sweep
	// touches is a cached read.
	if cfg.RetentionSweepInterval <= 0 {
		log.Warn("retention sweeper disabled (AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS=0) — the access_decision_log partition runway will not be extended, and rows will land in the default partition once the pre-created months elapse")
	} else {
		retentionCtx, stopRetention := context.WithCancel(context.Background())
		defer stopRetention()
		sweeper := retention.New(pgStore, log, cfg.RetentionSweepInterval,
			cfg.PartitionMonthsAhead, cfg.AccessDecisionRetentionMonths)
		go sweeper.Run(retentionCtx)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		log.Fatal("server error", zap.Error(err))
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	}

	// Stop consuming before the HTTP servers drain, so no message is applied
	// against a pool that is about to close.
	stopConsumer()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	if mtlsSrv != nil {
		if err := mtlsSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("mtls listener shutdown failed", zap.Error(err))
		}
	}
	log.Info("server stopped")
}

// kafkaErrorLogger adapts zap to kafka-go's LoggerFunc for a reader's
// ErrorLogger.
//
// ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
//
// kafka-go reports consumer-group problems — a failed JoinGroup, a metadata
// fetch that returned no partitions, a coordinator that moved — only through
// Logger and ErrorLogger. Both default to nil, which DISCARDS them.
//
// That was measured, not assumed: the lifecycle consumer joined its group,
// reported Stable with one member and ZERO partitions assigned, and consumed
// nothing, while the service log showed only "lifecycle consumer started".
// internal/events' Run loop cannot help — it logs errors returned from
// ReadMessage, and this failure never surfaces there: ReadMessage simply blocks
// forever on a member with no assignment.
//
// So a consumer that is doing nothing at all looks exactly like a consumer with
// nothing to do. On this service that means a suspension silently never taking
// effect. ErrorLogger is now wired on every reader.
//
// Only ErrorLogger, not Logger: kafka-go's Logger is per-fetch debug chatter
// on a hot loop, and turning it on would bury the errors this is here to
// surface.
func kafkaErrorLogger(log *zap.Logger, topics string) kafka.LoggerFunc {
	return func(msg string, args ...any) {
		log.Error("kafka consumer error",
			zap.String("topics", topics),
			zap.String("detail", fmt.Sprintf(msg, args...)))
	}
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}

// loadMTLSBootstrapToken reads the shared mTLS self-provisioning secret
// from a file (see deployments/docker-compose.yml's mtls-bootstrap-keygen)
// rather than an env var, so the value never appears in `docker inspect`/
// process-list output. Returns "" when unset or unreadable — the
// provisioning call this feeds then falls through to mtls-management-svc's
// normal principal/authorize path and fails loudly, the same way it would
// have before this bootstrap path existed.
func loadMTLSBootstrapToken(path string, log *zap.Logger) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("failed to read mtls bootstrap token file", zap.String("path", path), zap.Error(err))
		return ""
	}
	return strings.TrimSpace(string(data))
}
