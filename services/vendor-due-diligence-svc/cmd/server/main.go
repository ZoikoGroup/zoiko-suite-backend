package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/clients"
	"zoiko.io/vendor-due-diligence-svc/internal/config"
	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcenvelope "zoiko.io/vendor-due-diligence-svc/internal/envelope"
	"zoiko.io/vendor-due-diligence-svc/internal/events"
	"zoiko.io/vendor-due-diligence-svc/internal/handler"
	"zoiko.io/vendor-due-diligence-svc/internal/health"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
	"zoiko.io/vendor-due-diligence-svc/internal/mtls"
	"zoiko.io/vendor-due-diligence-svc/internal/store"
	"zoiko.io/vendor-due-diligence-svc/internal/telemetry"
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
// combinations doesn't grow the map unboundedly between reads of the same
// key.
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
// authorization-svc's /v1/authorize always responds 200, and signals the
// actual decision via decision_outcome: "GRANTED" | "DENIED" — there is no
// "allowed" boolean field in its response.
func (a *httpAuthzClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailabe
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailabe
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

	log.Info("vendor-due-diligence-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.String("counterparty_url", cfg.CounterpartyServiceURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "vendor-due-diligence-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("vendor-due-diligence-svc")

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

	// A nil writer selects the publisher's log-only path. That path existed all
	// along and was unreachable: KAFKA_BROKERS was read with a helper that treats
	// "set but empty" as unset, so an explicitly empty list was replaced by the
	// localhost default and the writer was always constructed.
	var kafkaWriter *kafka.Writer
	if len(cfg.Kafka.Brokers) > 0 {
		kafkaWriter = &kafka.Writer{
			Addr:     kafka.TCP(cfg.Kafka.Brokers...),
			Topic:    cfg.Kafka.Topic,
			Balancer: &kafka.LeastBytes{},
			// See employee-master-svc/spend-controls-svc: the broker has
			// KAFKA_AUTO_CREATE_TOPICS_ENABLE=true, but kafka-go's Writer
			// refuses to produce to a not-yet-existing topic unless this is
			// also set client-side — without it, every first publish to this
			// service's own topic fails with "Unknown Topic Or Partition".
			AllowAutoTopicCreation: true,
			// Without this every check costs an extra second. kafka-go batches and
			// BatchTimeout defaults to 1s: a synchronous write of a single message
			// waits for the batch to fill (100 messages) or for that timer,
			// whichever comes first. These events fire one per check, so the batch
			// never fills and the timer always wins — and both publishes happen on
			// the request path, so the caller pays for it twice.
			BatchTimeout: 10 * time.Millisecond,
		}
		defer func() { _ = kafkaWriter.Close() }()
	} else {
		log.Warn("no Kafka brokers configured — screening events will be logged, not published",
			zap.String("consequence", "vendor.dd.started, vendor.dd.completed and vendor.dd.failed reach no consumer"))
	}

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)

	var httpClientForAuthz *http.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "vendor-due-diligence-svc", "00000000-0000-0000-0000-00000000f001")
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
	counterpartyClient := clients.New(cfg.CounterpartyServiceURL)

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("vendor-due-diligence-svc", otelchi.WithChiRoutes(r)))
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

	h := handler.New(pgStore, publisher, authzClient, counterpartyClient, log)
	handler.RegisterRoutes(r, h)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
	healthH := health.New(pool, log)
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
