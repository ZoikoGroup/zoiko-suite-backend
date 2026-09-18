// Command server is the entrypoint for identity-context-svc.
//
// Tier 0 — must be running before any domain or governance service starts.
// See docs/architecture/06-blueprint.md Phase 1 exit criteria.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/authz"
	"zoiko.io/identity-context-svc/internal/config"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
	svcenvelope "zoiko.io/identity-context-svc/internal/envelope"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/health"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/retention"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/sod"
	"zoiko.io/identity-context-svc/internal/store"
	"zoiko.io/identity-context-svc/internal/telemetry"
	"zoiko.io/identity-context-svc/internal/upstream"
)

// kafkaWriterAdapter bridges the relay's broker-agnostic MessageWriter to
// kafka-go's concrete *kafka.Writer.
//
// The adapter exists so package outbox does not import kafka-go: a test fake
// for the relay would otherwise drag the whole broker client in with it, and
// the relay's logic — claim, publish, mark, back off — has nothing to do with
// which broker is on the other end.
type kafkaWriterAdapter struct{ w *kafka.Writer }

func (a kafkaWriterAdapter) WriteMessages(ctx context.Context, msgs ...outbox.KafkaMessage) error {
	out := make([]kafka.Message, len(msgs))
	for i, m := range msgs {
		// Topic is set on the Writer itself, not per message — kafka-go
		// rejects a Message that also specifies one when the Writer has it.
		out[i] = kafka.Message{Key: m.Key, Value: m.Value}
	}
	return a.w.WriteMessages(ctx, out...)
}

func main() {
	// ── Logger (structured JSON, production-grade) ────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	// ── Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}

	// ── Redis client ──────────────────────────────────────────────────────
	redisOpts, err := redisOptions(cfg.Redis)
	if err != nil {
		log.Fatal("Redis configuration invalid", zap.Error(err))
	}
	rdb := redis.NewClient(redisOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// Tier 0 — Redis is a hard dependency. Fail fast on startup.
		//
		// tls is logged because it is the field that is wrong when a managed
		// endpoint times out here: the server closes a cleartext connection
		// without ever answering, so the symptom is a timeout and not the
		// authentication failure the missing TLS really is.
		log.Fatal("Redis unreachable on startup — aborting",
			zap.String("addr", redisOpts.Addr),
			zap.Bool("tls", redisOpts.TLSConfig != nil),
			zap.Error(err),
		)
	}
	log.Info("Redis connection established",
		zap.String("addr", redisOpts.Addr),
		zap.Bool("tls", redisOpts.TLSConfig != nil),
	)

	// ── Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "identity-context-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("identity-context-svc")

	// GOV-01 instruments. These are the numbers the golden-signals dashboard
	// cannot infer from request rates — most importantly the outbox depth,
	// because a stopped relay produces no errors and no latency while every
	// governance event this service emits piles up in a table.
	govMetrics := telemetry.NewGovMetrics("identity-context-svc")

	// ── Postgres pool ─────────────────────────────────────────────────────
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
		// Tier 0 — Postgres is a hard dependency. Fail fast on startup.
		log.Fatal("Postgres unreachable on startup — aborting", zap.Error(err))
	}
	log.Info("Postgres connection established", zap.String("db_name", cfg.DB.Name))

	// ── Kafka producer ────────────────────────────────────────────────────
	// Connects lazily on first write — unlike Postgres/Redis this is not a
	// fail-fast startup dependency. Publish failures are handled per-call by
	// the resolver's existing error-return/log path (Gap 1 fix).
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
		// BatchTimeout is how long the writer holds a PARTIALLY FULL batch
		// before flushing it, and kafka-go's default is one second. Every
		// synchronous WriteMessages that does not fill a batch therefore
		// waits a second before returning. That is the whole cost: the
		// outbox relay measured 1.03 events/second against a 14,800-event
		// backlog until the relay was changed to send its batch in one call
		// and this was brought down to match.
		//
		// 20ms bounds the wait for the batches that do not fill — the tail of
		// a drain, and the single-event publishes on the resolve path.
		BatchTimeout: 20 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	// ── Domain dependencies ───────────────────────────────────────────────
	principalRepo := store.New(pool, log)
	riskCache := session.NewRiskSignalCache(rdb)

	// Redis is the hot cache; Postgres session_contexts is the durable evidence
	// record (migration 000005). DurableCache composes them and is what carries
	// the tenant scoping — a Redis session key names no owner, so the handlers
	// cannot enforce isolation on the cache alone.
	redisSessions := session.NewCache(rdb, cfg.Redis.SessionTTLSeconds)
	sessionCache := session.NewDurableCache(redisSessions, principalRepo, log)

	// ── Kafka consumer ────────────────────────────────────────────────────
	// Revocation events reach the estate through here: authority.revoked,
	// role.updated and entity.updated all invalidate live sessions, and
	// session.risk.changed is the ONLY writer of the risk cache the resolver
	// reads. None of it ran before, because this reader was never constructed
	// and the Consumer was unreachable code.
	//
	// Started before the HTTP listener so a revocation published while the
	// service was down is consumed rather than skipped. An absent broker is not
	// fatal — the reader retries in the background and authentication, which
	// needs no Kafka, serves throughout.
	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	defer stopConsumer()

	kafkaReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: cfg.Kafka.Brokers,
		Topic:   cfg.Kafka.Topic,
		GroupID: cfg.Kafka.GroupID,
		// Errors are surfaced through ReadMessage rather than kafka-go's own
		// logger, so the consumer decides the level. Without this a missing
		// broker prints to stderr once per dial attempt, outside zap.
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
			log.Debug("kafka reader: " + fmt.Sprintf(msg, args...))
		}),
	})
	consumer := events.NewConsumer(log, sessionCache, principalRepo, riskCache, principalRepo, events.NewRedisDeduper(rdb))
	go consumer.Run(consumerCtx, kafkaReader)
	upstreamRegistry := upstream.NewRegistryClient(cfg, log)

	// ── Transactional outbox ──────────────────────────────────────────────
	//
	// The publisher no longer writes to Kafka. It writes to event_outbox, in
	// the same Postgres the business facts land in, and the relay drains that
	// to the broker. See package outbox: the previous fire-and-forget
	// goroutines lost events on a broker blip or a SIGTERM, with the business
	// write already committed and the caller already holding an envelope.
	outboxStore := outbox.NewStore(pool, log)
	publisher := events.NewPublisher(log, cfg.Kafka.Topic, outboxStore)

	relayCfg := outbox.DefaultRelayConfig()
	relayCfg.BatchSize = cfg.OutboxRelayBatchSize
	relayCfg.PollInterval = time.Duration(cfg.OutboxRelayPollMillis) * time.Millisecond
	relay := outbox.NewRelay(pool, kafkaWriterAdapter{kafkaWriter}, relayCfg, log)

	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go relay.Run(relayCtx)

	// Sampled rather than incremented at the enqueue site: the depth is a
	// property of the TABLE, and several replicas share one outbox. A per-process
	// counter would miss a backlog left behind by a replica that has since been
	// replaced, which is exactly the case worth alerting on.
	go telemetry.SampleOutbox(relayCtx, relay, govMetrics, 30*time.Second, log)

	verifier := auth.NewJWTVerifier(cfg)
	signer, err := auth.NewJWTSigner(cfg)

	if err != nil {
		log.Fatal("failed to initialise JWT signer", zap.Error(err))
	}

	// ── Password hashing ───────────────────────────────────────────────────
	// Built before the server starts listening: NewHasher performs one
	// derivation to precompute its timing decoy, and misconfigured cost
	// factors must abort startup rather than silently hashing every password
	// weakly for the life of the process.
	hasher, err := credential.NewHasher(credential.Params{
		MemoryKiB:   uint32(cfg.ArgonMemoryKiB),
		Iterations:  uint32(cfg.ArgonIterations),
		Parallelism: uint8(cfg.ArgonParallelism),
		SaltLength:  credential.DefaultParams().SaltLength,
		KeyLength:   credential.DefaultParams().KeyLength,
	}, cfg.ArgonMaxConcurrent)
	if err != nil {
		log.Fatal("failed to initialise password hasher", zap.Error(err))
	}
	log.Info("password hasher ready",
		zap.Int("argon2_memory_kib", cfg.ArgonMemoryKiB),
		zap.Int("argon2_iterations", cfg.ArgonIterations),
		zap.Int("max_concurrent", cfg.ArgonMaxConcurrent),
		zap.Int("peak_memory_mib", (cfg.ArgonMemoryKiB*cfg.ArgonMaxConcurrent)/1024),
	)

	idpIssuer, err := auth.NewIdPTokenIssuer(cfg)
	if err != nil {
		log.Fatal("failed to initialise IdP token issuer", zap.Error(err))
	}

	siemClient := siem.New(cfg.SIEMServiceURL, "identity-context-svc", log)

	// Drain accepted SIEM events on shutdown. Stream returns before delivery,
	// so without this a SIGTERM would discard security events already accepted.
	defer siemClient.Close()

	// ── AuthZ client ───────────────────────────────────────────────────────
	// Fail fast rather than starting and 503-ing every guarded route. An empty
	// base URL builds requests that always fail, so CheckAllowed refuses —
	// correctly, but at request time rather than at boot, which hides a
	// deployment mistake until a user finds it.
	if cfg.AuthzServiceURL == "" {
		log.Fatal("AUTHZ_SERVICE_URL is required: every authorized route would return 503 without it")
	}
	authzClient, err := authz.NewClient(cfg.AuthzEnv, cfg.AuthzServiceURL, log)
	if err != nil {
		log.Fatal("failed to initialize authz client", zap.Error(err))
	}

	// ── GOV-04 segregation of duties ──────────────────────────────────────
	// Paired with the authz client above: a deployment that wired a real
	// GOV-03 but a stubbed GOV-04 would satisfy "authorization enforced" while
	// having no segregation control at all. NewChecker refuses the stub in
	// staging and production for exactly that reason.
	sodChecker, err := sod.NewChecker(cfg.Environment, cfg.SoDServiceURL, log)
	if err != nil {
		log.Fatal("failed to initialize segregation-of-duties checker", zap.Error(err))
	}

	// ── Resolver ──────────────────────────────────────────────────────────
	resolver := identityctx.NewResolver(
		cfg,
		log,
		principalRepo,
		sessionCache,
		riskCache,
		upstreamRegistry,
		publisher,
		verifier,
		signer,
		siemClient,
	).
		// Negative path #2: an unknown or foreign ingress cannot fall back to
		// another tenant. The checker can only ever refuse — it never supplies
		// a tenant, so a forged host header grants nothing.
		WithIngressChecker(identityctx.NewIngressChecker(
			principalRepo,
			identityctx.IngressPolicy(cfg.IngressPolicy),
			log,
		)).
		// Residency has been recorded on every session since migration 000005
		// and never enforced. Empty allow-list keeps enforcement off.
		WithResidencyPolicy(identityctx.NewResidencyPolicy(
			cfg.DeploymentRegion,
			cfg.AllowedResidencyPolicies,
			log,
		)).
		WithRetention(time.Duration(cfg.SessionEvidenceRetentionDays) * 24 * time.Hour).
		WithMetrics(govMetrics)

	// ── Authenticator ─────────────────────────────────────────────────────
	// The credential exchange that precedes resolution. principalRepo satisfies
	// both PrincipalStore (for the resolver) and CredentialStore (for this) —
	// they are separate interfaces over the same Postgres store because the
	// two paths have no business reaching each other's tables.
	authenticator := identityctx.NewAuthenticator(
		log,
		principalRepo,
		hasher,
		idpIssuer,
		publisher,
		siemClient,
		identityctx.LockoutPolicy{
			MaxFailedAttempts: cfg.AuthMaxFailedAttempts,
			LockDuration:      time.Duration(cfg.AuthLockDurationSeconds) * time.Second,
		},
	)

	// ── HTTP router ───────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Platform middleware
	r.Use(middleware.RequestID) // injects X-Request-Id
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer) // never let a panic crash the Tier 0 service
	r.Use(otelchi.Middleware("identity-context-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// Structured request logging
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)
			next.ServeHTTP(ww, req)
			log.Info("request",
				zap.String("method", req.Method),
				zap.String("path", req.URL.Path),
				zap.Int("status", ww.Status()),
				zap.Duration("duration", time.Since(start)),
				zap.String("correlation_id", req.Header.Get("X-Correlation-ID")),
			)
		})
	})

	// Health probe (no auth required).
	//
	// The outbox depth is the one number worth alerting on in this
	// architecture: everything else about the service can look healthy while
	// governance events pile up in a table, because a stopped relay is
	// invisible from the request path.
	//
	// The tenant registry probe closes the long-standing TODO here. It is a
	// fail-closed dependency of Dimension 2, so a registry outage means every
	// resolution 503s — the readiness probe should say what the request path
	// already knows rather than keeping the pod in rotation.
	r.Handle("/health", metrics.WrapReadinessHandler(
		health.NewHandler(rdb, pool).
			WithOutbox(relay, cfg.OutboxRelayBatchSize*100).
			WithUpstream(upstreamRegistry),
	))
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/.well-known/jwks.json", auth.NewJWKSHandler(signer.PublicKey(), cfg.JWTKeyID))

	// ── GOV-01 command services ───────────────────────────────────────────
	supportService := identityctx.NewSupportService(
		principalRepo,
		publisher,
		sodChecker,
		siemClient,
		identityctx.SupportPolicy{
			MaxTTL:                 time.Duration(cfg.SupportContextMaxTTLSeconds) * time.Second,
			DefaultTTL:             time.Duration(cfg.SupportContextDefaultTTLSeconds) * time.Second,
			MinJustificationLength: identityctx.DefaultSupportPolicy().MinJustificationLength,
		},
		log,
	).WithMetrics(govMetrics)

	cacheService := identityctx.NewContextCacheService(
		principalRepo,
		sessionCache,
		publisher,
		siemClient,
		log,
	)

	// Domain routes (all under /v1/)
	h := identityctx.NewHandler(resolver, authenticator, sessionCache, principalRepo, authzClient, log).
		WithEnvironment(domain.Environment(cfg.Environment)).
		WithSupport(supportService).
		WithContextCache(cacheService)
	identityctx.RegisterRoutes(r, h)

	// ── Retention / disposition sweep (GOV-09) ────────────────────────────
	//
	// Refuses to dispose of anything a GOV-10 legal hold covers, and reports
	// what it held back separately from what it disposed. See package
	// retention for why those two numbers must not be collapsed.
	retentionWorker := retention.New(principalRepo, publisher, retention.Config{
		Interval:        time.Duration(cfg.RetentionSweepIntervalMinutes) * time.Minute,
		BatchSize:       500,
		OutboxRetention: time.Duration(cfg.OutboxRetentionDays) * 24 * time.Hour,
	}, log).WithMetrics(govMetrics)
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	defer stopRetention()
	go retentionWorker.Run(retentionCtx)

	// ── Server ────────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info("identity-context-svc starting",
			zap.Int("port", cfg.Port),
			zap.String("tier", "0"),
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining connections")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("stopping event consumer")
	stopConsumer()
	log.Info("stopping retention worker")
	stopRetention()

	// ── Final outbox drain ────────────────────────────────────────────────
	//
	// The relay loop is stopped first so it is not competing for the same
	// rows, then ONE deterministic pass runs to deliver whatever the handlers
	// enqueued in their last seconds.
	//
	// This is best-effort by design, and it is fine that it is: unlike the old
	// goroutine drain, nothing is LOST if it fails. The events are committed
	// in Postgres and the next process to start delivers them. That is the
	// whole point of the outbox — shutdown stopped being a correctness
	// problem and became a latency one.
	log.Info("stopping outbox relay")
	stopRelay()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()

	if n, err := relay.DrainOnce(drainCtx); err != nil {
		log.Warn("final outbox drain incomplete — events remain durable and will be delivered on next start",
			zap.Error(err))
	} else if n > 0 {
		log.Info("final outbox drain delivered pending events", zap.Int("events", n))
	}
	if pending, err := relay.PendingCount(drainCtx); err == nil && pending > 0 {
		log.Info("outbox has undelivered events — they survive this shutdown",
			zap.Int("pending", pending))
	}

	// The remaining goroutines are the SIEM streams and the few event publishes
	// that still run detached — the enqueue is now a Postgres write rather than
	// a broker round trip, so these complete in milliseconds instead of
	// blocking on an unreachable Kafka. Bounded regardless: a stop that never
	// completes is indistinguishable from a crash in every dashboard that
	// watches for clean termination.
	log.Info("draining in-flight event goroutines")
	if err := resolver.Drain(drainCtx); err != nil {
		log.Warn("resolver drain incomplete", zap.Error(err))
	}
	if err := authenticator.Drain(drainCtx); err != nil {
		log.Warn("authenticator drain incomplete", zap.Error(err))
	}
	log.Info("identity-context-svc stopped")
}

// redisOptions turns RedisConfig into a go-redis client configuration.
//
// REDIS_URL wins outright when set. Managed providers issue one string that
// already carries host, port, credentials and — through the `rediss` scheme —
// TLS; splitting it back into four variables by hand is how one of the four
// ends up stale. Host/Port/Password/TLSEnabled remain for the local container
// and for deployments that inject the parts separately.
//
// Nothing here logs. Addr is safe for the caller to log because ParseURL keeps
// the password in Options.Password rather than in Addr; the URL itself is not.
func redisOptions(cfg config.RedisConfig) (*redis.Options, error) {
	if cfg.URL != "" {
		opts, err := redis.ParseURL(cfg.URL)
		if err != nil {
			// ParseURL echoes its input in the error, password included, so the
			// message is replaced rather than wrapped.
			return nil, errors.New("REDIS_URL is malformed — expected rediss://user:password@host:port")
		}
		return opts, nil
	}

	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Password: cfg.Password,
	}
	if cfg.TLSEnabled {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: cfg.Host,
		}
	}
	return opts, nil
}
