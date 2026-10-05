package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/config"
	"zoiko.io/delegated-authority-svc/internal/domain"
	svcenvelope "zoiko.io/delegated-authority-svc/internal/envelope"
	"zoiko.io/delegated-authority-svc/internal/events"
	"zoiko.io/delegated-authority-svc/internal/expiry"
	"zoiko.io/delegated-authority-svc/internal/handler"
	"zoiko.io/delegated-authority-svc/internal/health"
	"zoiko.io/delegated-authority-svc/internal/idempotency"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
	"zoiko.io/delegated-authority-svc/internal/mtls"
	"zoiko.io/delegated-authority-svc/internal/outbox"
	"zoiko.io/delegated-authority-svc/internal/store"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

// decisionCacheTTL bounds how long a GRANTED/DENIED decision from
// authorization-svc may be reused locally before it is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 anticipates exactly
// this cost: "For Tier 0 and latency-sensitive services, policy and
// authorization evaluation may use high-speed distributed enforcement
// patterns, including local policy caches... provided policy source
// remains centralized, policy provenance is auditable, stale decision
// risk is bounded, fail-safe behavior is defined." This constant is that
// bound — short enough that a permission revocation or role change
// propagates within one cache generation, long enough to absorb the
// repeat checks a single user action or request burst produces.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable or
// misbehaving authorization-svc is never cached — that would turn one
// transient outage into a standing permit-or-deny for every subsequent
// caller on this instance, which defeats fail-closed.
const decisionCacheTTL = 5 * time.Second

// platformScopeID mirrors authorization-svc's own constant of the same name.
//
// It is one literal shared by the whole estate and by
// deployments/scripts/seed-demo-rbac.ps1. A service that invents its own gets
// grants seeded against one id and checks made against another — silently, and
// fail-closed, so the symptom is a correctly-permissioned operator being
// refused with no indication why.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type httpAuthzClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func (a *httpAuthzClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := a.lookupCache(key); hit {
		return decision
	}

	err := a.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
		a.storeCache(key, err)
	}

	return err
}

// lookupCache returns the cached decision for key and whether it is still
// within decisionCacheTTL. An expired entry is evicted on read.
func (a *httpAuthzClient) lookupCache(key string) (error, bool) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	d, ok := a.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(d.expiresAt) {
		delete(a.cache, key)
		return nil, false
	}
	return d.deniedErr, true
}

// storeCache records a real GRANTED/DENIED decision. Every 1000th write
// sweeps expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations doesn't grow the map
// unboundedly between reads of the same key.
func (a *httpAuthzClient) storeCache(key string, decision error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	a.cache[key] = cachedDecision{deniedErr: decision, expiresAt: time.Now().Add(decisionCacheTTL)}

	a.cacheWrites++
	if a.cacheWrites%1000 == 0 {
		now := time.Now()
		for k, v := range a.cache {
			if now.After(v.expiresAt) {
				delete(a.cache, k)
			}
		}
	}
}

// checkAllowedLive is the real, uncached call to authorization-svc.
func (a *httpAuthzClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	return a.authorize(ctx, principalID, legalEntityID, actionType, nil, actionType)
}

// CheckAllowedAtLimit asks whether principalID may perform actionType at the
// given monetary ceiling: authorization-svc evaluates the principal's own
// authority limits against the amount (ORG-06 negative case 11, "delegator
// grants higher limit than own authority → reject"). The limit is required, so
// a principal with no limit configured for the action is refused rather than
// treated as unlimited. Never cached — the answer depends on the amount.
func (a *httpAuthzClient) CheckAllowedAtLimit(ctx context.Context, principalID, legalEntityID, actionType, amount, currency string) error {
	return a.authorize(ctx, principalID, legalEntityID, actionType, map[string]string{
		"amount":                   amount,
		"currency":                 currency,
		"authority_limit_required": "true",
	}, actionType+":limit:"+amount+currency)
}

func (a *httpAuthzClient) authorize(ctx context.Context, principalID, legalEntityID, actionType string, attributes map[string]string, idemSuffix string) error {
	body := map[string]any{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	}
	if len(attributes) > 0 {
		body["attributes"] = attributes
	}
	reqBody, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// authorization-svc validates the same canonical envelope contract this
	// service does and answers 400 envelope_incomplete without it; a non-200
	// is treated as unavailable below. One decision per (request, question).
	forwardEnvelope(ctx, req, principalID, legalEntityID, idemSuffix)
	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if res.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// Ping reports whether authorization-svc is reachable, for readiness.
//
// It asks /healthz rather than replaying an authorize call: a probe must not
// write entries into the access decision log, which is an append-only record of
// real decisions about real principals, and a synthetic check every few seconds
// would bury the real ones.
//
// The path is /healthz, not /health. identity-context-svc shipped a registry
// probe against /health for a service that serves /healthz — it returned 503
// permanently and its unit test asserted the wrong path, so the test encoded
// the defect instead of catching it. Verified against the running container
// here rather than against a stub.
func (a *httpAuthzClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authorization-svc /healthz returned %d", resp.StatusCode)
	}
	return nil
}

// httpSoDClient asks authorization-svc's SoD engine whether giving the
// delegate this action would combine duties that must stay apart (ORG-06 §4.6
// "cannot delegate around SoD").
//
// It speaks authorization-svc's real contract — POST /v1/sod/validate with
// candidate_actions and the principal whose CURRENT holdings are checked — and
// that principal is the DELEGATE: the question is whether the person receiving
// the authority would then hold a conflicting pair. It used to POST to
// /v1/sod/check, a route nothing serves, so every create answered 503; and it
// decoded a "conflict" field the engine never sends, so correcting the path
// alone would have read every answer as "no conflict". The answer is now
// decoded strictly: a response without conflict_free is unreadable, and an
// unreadable answer refuses (fail closed), never permits.
type httpSoDClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// CheckConflict implements handler.SoDClient.
func (s *httpSoDClient) CheckConflict(ctx context.Context, tenantID, legalEntityID, delegatorPrincipalID, delegatePrincipalID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]any{
		"candidate_actions": []string{actionType},
		"principal_id":      delegatePrincipalID,
		"legal_entity_id":   legalEntityID,
		"tenant_id":         tenantID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/sod/validate", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The caller asks; the delegator is only the fallback when the envelope
	// carries no actor (service-to-service paths).
	caller := delegatorPrincipalID
	if env, ok := svcenvelope.FromContext(ctx); ok && env.ActorSubjectID != "" {
		caller = env.ActorSubjectID
	}
	forwardEnvelope(ctx, req, caller, legalEntityID, "sod:"+actionType)
	if req.Header.Get("X-Tenant-Id") == "" && tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Error("failed to call authorization-svc SoD validate", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.log.Error("authorization-svc SoD validate returned non-200", zap.Int("status", resp.StatusCode))
		return domain.ErrAuthzServiceUnavailable
	}
	var res struct {
		ConflictFree *bool `json:"conflict_free"`
		Conflicts    []struct {
			CandidateAction string `json:"candidate_action"`
			ConflictsWith   string `json:"conflicts_with"`
			Source          string `json:"source"`
		} `json:"conflicts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil || res.ConflictFree == nil {
		s.log.Error("authorization-svc SoD validate answered without conflict_free; refusing", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	if !*res.ConflictFree {
		if len(res.Conflicts) > 0 {
			c := res.Conflicts[0]
			return fmt.Errorf("%w: %s conflicts with %s (%s)", domain.ErrSODConflict, c.CandidateAction, c.ConflictsWith, c.Source)
		}
		return domain.ErrSODConflict
	}
	return nil
}

// forwardEnvelope sets the canonical §4 envelope authorization-svc validates
// on every call. The values are the CALLER's, from the envelope the middleware
// already parsed, so a decision is traceable to the request that caused it.
// idemSuffix makes one Idempotency-Key per (request, question).
func forwardEnvelope(ctx context.Context, req *http.Request, principalID, legalEntityID, idemSuffix string) {
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	requestID := middleware.GetReqID(ctx)
	sourceChannel := "system"
	if requestID == "" {
		// A background path has no inbound request; an empty id is a 400 at
		// authorization-svc, so mint one rather than send none.
		requestID = uuid.NewString()
	}
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	if req.Header.Get("X-Correlation-ID") == "" {
		req.Header.Set("X-Correlation-ID", requestID)
	}
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Source-Channel", sourceChannel)
	req.Header.Set("Idempotency-Key", requestID+":"+idemSuffix)
}

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

	log.Info("delegated-authority-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "delegated-authority-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("delegated-authority-svc")
	domainMetrics := telemetry.NewDomain("delegated-authority-svc")

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

	// ── 4. Store, Kafka producer, clients ─────────────────────────────────────
	pgStore := store.New(pool)

	kafkaWriter := &kafka.Writer{
		Addr:     kafka.TCP(cfg.Kafka.Brokers...),
		Topic:    cfg.Kafka.Topic,
		Balancer: &kafka.LeastBytes{},
		// The broker has KAFKA_AUTO_CREATE_TOPICS_ENABLE=true, but kafka-go's
		// Writer refuses to produce to a topic it doesn't already know about
		// unless this is also set client-side — without it, every publish to
		// a not-yet-existing topic fails with "Unknown Topic Or Partition"
		// even though the broker would have created it.
		AllowAutoTopicCreation: true,
		// The library default is 1s, so every delegation event sat in the
		// writer's batch buffer for a full second before it left the process.
		// On this service that delay is not cosmetic: authority.revoked is the
		// signal that tells identity-context-svc to invalidate a session, so a
		// revocation the operator has already been told succeeded stayed
		// invisible to the consumer that acts on it.
		BatchTimeout: 10 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	// mTLS to authorization-svc, off by default and identical to the siblings'
	// wiring. Every call this service makes to authorization-svc decides
	// whether somebody may hand another principal their authority, which is
	// exactly the traffic the material-path rollout exists for.
	var httpClientForAuthz *http.Client
	authzBaseURL := cfg.AuthZServiceURL
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "delegated-authority-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		httpClientForAuthz = mtlsHTTPClient
		authzBaseURL = cfg.AuthzMTLSURL
	} else {
		httpClientForAuthz = &http.Client{Timeout: 5 * time.Second}
	}
	authzClient := &httpAuthzClient{baseURL: authzBaseURL, client: httpClientForAuthz, log: log, cache: make(map[string]cachedDecision)}
	// SoD client reuses the same HTTP client and base URL as the authz client.
	sodClient := &httpSoDClient{baseURL: authzBaseURL, client: httpClientForAuthz, log: log}

	// ── 4b. Outbox relay ──────────────────────────────────────────────────────
	//
	// Events are written by the store inside the transaction that changes the
	// state; this loop is what delivers them. Started before the server so a
	// backlog left by the previous process is already draining when the first
	// request arrives.
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	relay := outbox.NewRelay(pgStore, publisher, domainMetrics, log)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.Run(relayCtx)
	}()

	// ── 4c. Expiry sweeper ────────────────────────────────────────────────────
	//
	// Ends delegations whose window has closed, in every tenant, without
	// waiting for somebody to read the register. The read paths still sweep
	// their own tenant so a register read never shows a lapsed grant as ACTIVE;
	// this loop is what makes authority.expired timely and its coverage
	// complete. Started alongside the relay so the events it enqueues are
	// drained by a relay that is already running.
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	defer sweeperCancel()
	sweeper := expiry.New(pgStore, domainMetrics, log).WithInterval(cfg.ExpirySweepInterval)
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		sweeper.Run(sweeperCtx)
	}()

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("delegated-authority-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// Idempotency-Key replay (cross-service finding 2): after the envelope has
	// resolved tenant and actor, before any handler runs the command.
	r.Use(idempotency.Middleware(pgStore, log))

	h := handler.New(pgStore, authzClient, sodClient, log, domainMetrics)
	handler.RegisterRoutes(r, h)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
	// authorization-svc is a readiness dependency, not merely a runtime one.
	// Every endpoint here calls it before doing anything and this service fails
	// closed, so with it unreachable the pool can be perfectly healthy while
	// 100% of requests answer 503. Readiness used to report ready throughout
	// exactly that outage.
	healthH := health.New(pool, log, health.Dependency{Name: "authorization-svc", Check: authzClient.Ping})
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 7. HTTP server with graceful shutdown ─────────────────────────────────
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

	// Stop the relay AFTER the server, and wait for it.
	//
	// The order is the point: in-flight requests are still committing events
	// while Shutdown drains them, so a relay stopped first would leave that last
	// handful of authority.revoked rows sitting until the next process starts.
	// They would not be lost — that is what the outbox is for — but a revocation
	// should not wait on a deployment.
	// Stop the sweeper BEFORE the relay, and wait for it.
	//
	// Same ordering argument one link further down the chain: a sweep pass that
	// is mid-flight is committing authority.expired rows, and stopping the
	// relay first would leave exactly those sitting until the next process
	// starts. Stopping the producer before its consumer means the relay's own
	// shutdown below drains whatever the last pass produced.
	sweeperCancel()
	select {
	case <-sweeperDone:
	case <-time.After(10 * time.Second):
		log.Warn("expiry sweeper did not stop within 10s; due delegations remain ACTIVE and will be expired at next start")
	}

	relayCancel()
	select {
	case <-relayDone:
	case <-time.After(10 * time.Second):
		log.Warn("outbox relay did not stop within 10s; undelivered events remain queued and will be drained at next start")
	}
	log.Info("server stopped")
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
