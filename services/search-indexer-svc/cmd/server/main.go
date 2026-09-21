// search-indexer-svc is ZoikoSuite's secure search control plane
// (ZS-SVC-AB-001, ESR-01 through ESR-05).
//
// WHAT CHANGED, AND WHY IT HAD TO.
//
// This service used to poll obligations-svc over HTTP every 60 seconds and
// upsert whatever came back into OpenSearch. That design could never work, and
// the backend completion tracker records it as row 65a: fetchObligations
// called GET /v1/obligations with NO HEADERS AT ALL, and that endpoint
// requires both X-Principal-Id and X-Tenant-Id — so every sync cycle failed
// 401 and the obligations index was never populated. The service reported
// healthy throughout.
//
// The tempting fix — forward a tenant header — does not work either, because
// the syncer was deliberately cross-tenant: it polled every obligation and
// resolved each one's tenant afterwards, and "all tenants" is not expressible
// in one tenant header. Worse, the resolution step had the SAME defect one
// level down: it called tenant-entity-registry-svc's GET /v1/entities/{id}
// headerless, and that endpoint scopes its query by the caller's tenant and
// answers 404 without one. Both halves were broken, and fixing either by
// minting a privileged cross-tenant read would have invented exactly the
// platform-scope surface two tiers of isolation work had just removed.
//
// The documented architecture already answers this. Doc 03 §37 and Doc 04
// §9.8/§556/§620 place search indexes as event-driven derivative projections —
// "search is derivative, never authoritative" — consuming domain events that
// already carry tenant_id in their envelope, and therefore needing no
// cross-tenant read privilege at all. This service now does that: it consumes
// the event backbone, takes the trusted tenant from the envelope, and refuses
// to project anything that does not carry one (INV-02).
//
// One producer-side gap had to be closed for that to work: obligations-svc's
// event envelope carried legal_entity_id but not tenant_id, so its events
// could not be projected. That is fixed in obligations-svc, not worked around
// here — a resolution lookup in this service would have reintroduced the
// privileged read.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/config"
	svcenvelope "zoiko.io/search-indexer-svc/internal/envelope"
	"zoiko.io/search-indexer-svc/internal/events"
	"zoiko.io/search-indexer-svc/internal/handler"
	"zoiko.io/search-indexer-svc/internal/health"
	"zoiko.io/search-indexer-svc/internal/indexer"
	svckafka "zoiko.io/search-indexer-svc/internal/kafka"
	"zoiko.io/search-indexer-svc/internal/query"
	"zoiko.io/search-indexer-svc/internal/retrieval"
	"zoiko.io/search-indexer-svc/internal/store"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

const serviceName = "search-indexer-svc"

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	if err := run(log); err != nil {
		log.Fatal("startup failed", zap.Error(err))
	}
}

func run(log *zap.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── Telemetry ────────────────────────────────────────────────────────
	metrics := telemetry.NewMetrics(serviceName)
	if cfg.OTELExporterEndpoint != "" {
		shutdown, err := telemetry.InitTracing(ctx, serviceName, cfg.OTELExporterEndpoint)
		if err != nil {
			// Not fatal. Tracing is observability; refusing to start over it
			// would mean an OTLP collector outage takes search down with it.
			log.Warn("tracing disabled — OTLP exporter could not be created", zap.Error(err))
		} else {
			defer func() {
				shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutCancel()
				_ = shutdown(shutCtx)
			}()
		}
	}

	// ── Postgres ─────────────────────────────────────────────────────────
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		return fmt.Errorf("parse database config: %w", err)
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
	poolCfg.MaxConns = 10
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	log.Info("database connected", zap.String("database", cfg.DB.Name))

	st := store.New(pool, log)

	// ── OpenSearch ───────────────────────────────────────────────────────
	engine, err := searchclient.NewEngine(searchclient.Config{
		Addresses: cfg.OpenSearch.Addresses,
		Username:  cfg.OpenSearch.Username,
		Password:  cfg.OpenSearch.Password,
	})
	if err != nil {
		return fmt.Errorf("create search engine client: %w", err)
	}

	// ── authorization-svc ────────────────────────────────────────────────
	// Refuses to build the permit-all stub outside local development, so a
	// misconfigured deployment does not come up serving unauthorized results.
	azClient, err := authz.NewClient(cfg.Env, cfg.AuthZServiceURL, log)
	if err != nil {
		return err
	}

	// ── Kafka ────────────────────────────────────────────────────────────
	var eventWriter events.MessageWriter = events.NopPublisher{}
	var kafkaWriter *kafka.Writer
	if len(cfg.Kafka.Brokers) > 0 {
		kafkaWriter = &kafka.Writer{
			Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
			Topic:                  events.Topic,
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
		}
		defer func() { _ = kafkaWriter.Close() }()
		eventWriter = kafkaWriter
	}
	publisher := events.NewPublisher(log, eventWriter)

	// ── Domain wiring ────────────────────────────────────────────────────
	ix := indexer.New(st, engine, publisher, metrics, log)
	if err := ix.Reload(ctx); err != nil {
		// Fatal, unlike the telemetry failures above. Without the registry
		// this process does not know which generation is live for any scope,
		// so every projection would go to the wrong place and every search
		// would resolve against a stale alias. Coming up in that state is
		// worse than not coming up.
		return fmt.Errorf("load projector registry: %w", err)
	}

	planner := query.NewPlanner(query.Limits{
		MaxResultWindow:    cfg.MaxResultWindow,
		MaxComplexityScore: cfg.MaxComplexityScore,
		FacetMinCount:      cfg.FacetMinCount,
		MaxPages:           100,
		MinQueryLength:     2,
	}, cfg.CursorSigningKey)

	// Hydrator is nil in this deployment: no scope is registered R2 yet, and
	// a hydrator wired to nothing would be worse than none — the retriever
	// answers ESR-014 for an R2 scope with no hydrator, which is a loud and
	// correct refusal, where a hydrator that returned index content would be
	// a silent freshness lie.
	retriever := retrieval.New(engine, azClient, nil, metrics, log, cfg.SourceHydrationTimeout)

	runner := svckafka.NewRunner(cfg.Kafka.Brokers, cfg.Kafka.GroupID, ix, metrics, log)
	defer runner.Close()

	topics := ix.Topics()
	for _, t := range cfg.Kafka.BootstrapTopics {
		topics = append(topics, t)
	}
	runner.Subscribe(ctx, topics)

	// Re-subscribe periodically so a source registered through the API starts
	// being consumed without a restart. The registry reload happens inline on
	// the write; this loop is what turns the reloaded topic set into actual
	// readers.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runner.Subscribe(ctx, ix.Topics())
			}
		}
	}()

	go ix.RunSweeps(ctx, cfg.RestrictionVerifyInterval, cfg.CheckpointInterval)

	// ── Health ───────────────────────────────────────────────────────────
	healthH := health.New(2*time.Second,
		health.Checker{Name: "postgres", Critical: true, Probe: pool.Ping},
		health.Checker{Name: "opensearch", Critical: true, Probe: engine.Ping},
		health.Checker{
			Name: "alias_consistency", Critical: false,
			// §8.3's alias-drift detection. Non-critical because drift means
			// the alias and the control plane disagree about WHICH generation
			// is serving — the service is still answering, and taking the
			// instance out of rotation would replace a correctness incident
			// with an availability one. It is surfaced in the body so the
			// probe is the thing that tells an operator to open the incident.
			Probe: func(ctx context.Context) error { return checkAliasDrift(ctx, st, engine) },
		},
	)
	healthH.SetReady(true)

	// ── HTTP ─────────────────────────────────────────────────────────────
	h := handler.New(handler.Config{
		Store: st, Engine: engine, Planner: planner, Retriever: retriever,
		Indexer: ix, AuthZ: azClient, Events: publisher, Metrics: metrics, Log: log,
		PlatformScopeID: cfg.AuthZPlatformScopeID,
		// The cursor key doubles as the actor-hash key. One secret rather than
		// two: both are HMAC keys with the same rotation story, and a second
		// environment variable is a second thing to leave unset.
		EvidenceKey: cfg.CursorSigningKey,
	})

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(otelchi.Middleware(serviceName, otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationID)

	// Probes and metrics are mounted BEFORE the envelope middleware. A
	// liveness probe cannot carry a tenant or an actor, and gating it on the
	// canonical input contract would make the orchestrator's health check the
	// first thing to fail.
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	r.Group(func(r chi.Router) {
		r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(),
			func(req *http.Request, e svcenvelope.Envelope, verr *svcenvelope.ValidationError) {
				log.Warn("canonical input contract violated",
					zap.String("path", req.URL.Path),
					zap.String("method", req.Method),
					zap.Error(verr))
			}))
		h.Routes(r)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: r,
		// Generous relative to a CRUD service: a governed search
		// re-authorizes every hit on a page, and each of those is a network
		// call to authorization-svc. The per-call timeouts inside the authz
		// client are the real bound; this one only has to be longer than the
		// worst legitimate page.
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("search-indexer-svc listening",
			zap.Int("port", cfg.Port),
			zap.Strings("topics", topics),
			zap.Strings("live_scopes", ix.LiveScopes()))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("HTTP server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", zap.Error(err))
	}
	return nil
}

// checkAliasDrift compares every scope's ACTIVE generation against what its
// alias actually resolves to.
//
// NP-42 and §8.3: "detect serving generation differs from desired signed
// generation; freeze automated cutover and open incident." The control plane
// cannot produce this disagreement on its own — the partial unique index on
// ACTIVE and the activate-engine-then-write order both prevent it — so drift
// found here came from outside this service, which is exactly the case the
// spec wants an incident for.
func checkAliasDrift(ctx context.Context, st store.Store, engine searchclient.Engine) error {
	contracts, err := st.ListContracts(ctx, "")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range contracts {
		if seen[c.ScopeName] {
			continue
		}
		seen[c.ScopeName] = true

		gen, err := st.GetActiveGeneration(ctx, c.ScopeName)
		if err != nil {
			// No active generation is not drift; it is a scope that has never
			// been built.
			continue
		}
		target, err := engine.ActiveGeneration(ctx, searchclient.Alias(searchclient.IndexName(c.ScopeName)))
		if err != nil {
			return fmt.Errorf("scope %s: %w", c.ScopeName, err)
		}
		if target != gen.PhysicalIndex {
			return fmt.Errorf(
				"scope %s: alias serves %q but the control plane records %q as ACTIVE",
				c.ScopeName, target, gen.PhysicalIndex)
		}
	}
	return nil
}

// correlationID ensures every request carries one and echoes it back.
func correlationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-ID")
		if id == "" {
			id = chimiddleware.GetReqID(r.Context())
			r.Header.Set("X-Correlation-ID", id)
		}
		w.Header().Set("X-Correlation-ID", id)
		next.ServeHTTP(w, r)
	})
}
