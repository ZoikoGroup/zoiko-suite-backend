package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/config"
	"zoiko.io/access-control-svc/internal/domain"
	svcenvelope "zoiko.io/access-control-svc/internal/envelope"
	"zoiko.io/access-control-svc/internal/events"
	"zoiko.io/access-control-svc/internal/handler"
	"zoiko.io/access-control-svc/internal/health"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/mtls"
	"zoiko.io/access-control-svc/internal/store"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

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

// authorization-svc's /v1/authorize always responds 200, and signals the
// actual decision via decision_outcome: "GRANTED" | "DENIED" — there is no
// "allowed" boolean field in its response.
//
// ── THE ENVELOPE IS NOT OPTIONAL ON THIS CALL ───────────────────────────────
//
// This function used to send Content-Type and nothing else. authorization-svc
// enforces the canonical input contract in middleware, ahead of the handler,
// so every call was rejected with 401 `envelope_incomplete` before any
// decision was evaluated. The non-200 then became ErrAuthzServiceUnavailable
// below, and every route in this service answered 503 `authz_unavailable`.
//
// That failure was indistinguishable from authorization-svc being down. It was
// not down — it was answering, correctly, that the request was malformed. The
// symptom was a service that could not create, read or update a single role
// definition while reporting a governance-plane outage.
//
// Nothing needed to be threaded through the AuthzClient interface to fix it:
// the tenant is already in the request context (svcmiddleware.TenantContext
// installs it, and requireTenant reads it the same way), and chi's RequestID
// middleware supplies the correlation id that main's own middleware already
// forwards on the inbound side.
//
// X-Purpose-Context is deliberately NOT sent. authorization-svc does not
// require it on this route — verified against the running service — and this
// call carries no personal, bank, tax or payroll content of its own. Sending
// an invented purpose to satisfy a check that is not being made would put a
// false provenance claim into the access decision log.
func (a *httpAuthzClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)

	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
		// Also in the body because authorization-svc's resolveTenantScope
		// accepts it there as a fallback. The header is what it prefers and
		// what it verifies against; a body value that CONTRADICTS the header
		// is refused outright, so sending the same value in both is safe and
		// keeps the call working if the header is ever dropped upstream.
		"tenant_id": tenantID,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// The verified scope. An empty tenant here is not fatal to the call —
	// authorization-svc warns and evaluates only globally-applicable SoD
	// rules — but it IS a weaker check than intended, so it is logged rather
	// than passed over in silence.
	if tenantID == "" {
		a.log.Warn("authorize: no tenant in context — authorization-svc will evaluate only global SoD rules",
			zap.String("action_type", actionType))
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	// Correlation joins this decision to the inbound request that caused it,
	// which is what makes an access_decision_log row traceable back to an
	// operator action. GetReqID returns "" outside a chi RequestID scope
	// (notably in tests calling the client directly), so fall back rather
	// than send an empty header the middleware will reject.
	correlationID := middleware.GetReqID(ctx)
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)

	// Per-hop, distinct from the correlation id by design: one operator action
	// can produce several authorize calls (a role write checks ROLE_MANAGE,
	// a listing checks ROLE_VIEW) and each is its own request.
	req.Header.Set("X-Request-Id", uuid.NewString())

	// This is service-to-service, not a user channel. "system" is one of the
	// values authorization-svc's contract admits.
	req.Header.Set("X-Source-Channel", "system")

	// Required as duplicate/replay protection (INV-08) because an authorize
	// call is not read-only — it appends to access_decision_log. A fresh key
	// per call is correct: two identical checks are two real decisions and
	// both belong in the evidence record.
	req.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Log the status and body. This is the branch that silently turned a
		// contract violation into a reported outage for as long as the
		// envelope was missing; without the body, the next such mismatch is
		// just as invisible as this one was.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		a.log.Error("authorization-svc refused the authorize call",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", actionType),
			zap.String("correlation_id", correlationID),
			zap.ByteString("body", body))
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

	log.Info("access-control-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "access-control-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("access-control-svc")

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
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)

	var httpClientForAuthz *http.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "access-control-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		httpClientForAuthz = mtlsHTTPClient
	} else {
		httpClientForAuthz = &http.Client{Timeout: 5 * time.Second}
	}
	authzBaseURL := cfg.AuthZServiceURL
	if cfg.AuthzMTLSEnabled {
		authzBaseURL = cfg.AuthzMTLSURL
	}
	authzClient := &httpAuthzClient{baseURL: authzBaseURL, client: httpClientForAuthz, log: log, cache: make(map[string]cachedDecision)}
	authzAdminClient := clients.NewAuthzAdminClient(cfg.AuthZServiceURL)

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("access-control-svc", otelchi.WithChiRoutes(r)))
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

	h := handler.New(pgStore, publisher, authzClient, authzAdminClient, log)
	handler.RegisterRoutes(r, h)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 7. HTTP server with graceful shutdown ─────────────────────────────────
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

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
