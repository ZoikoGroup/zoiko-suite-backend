// Package main is the entry point for obligations-svc.
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
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	"zoiko.io/obligations-svc/internal/authz"
	"zoiko.io/obligations-svc/internal/config"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
	"zoiko.io/obligations-svc/internal/events"
	"zoiko.io/obligations-svc/internal/handler"
	"zoiko.io/obligations-svc/internal/health"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	svcmiddleware "zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/outbox"
	"zoiko.io/obligations-svc/internal/registry"
	"zoiko.io/obligations-svc/internal/store"
	"zoiko.io/obligations-svc/internal/telemetry"
)

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

	log.Info("obligations-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("jurisdiction_rules_url", cfg.JurisdictionRulesURL),
	)

	// ── 2b. Tracing (Observability Baseline, 03-microservices.md §3.8) ─────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "obligations-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("obligations-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	// Tier 0 pool sizing — same values as policy-svc/jurisdiction-rules-svc/
	// tenant-entity-registry-svc.
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

	// ── 4. Store, Kafka producer, jurisdiction validator ─────────────────────
	pgStore := store.New(pool, log)

	// Kafka producer — connects lazily on first write, same posture as
	// identity-context-svc/tenant-entity-registry-svc/policy-svc: not a
	// fail-fast startup dependency like Postgres.
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
		// kafka-go's default is 1s, and WriteMessages is synchronous — so
		// every obligation write paid a wasted second inside the request,
		// waiting out a batch window for a single message. Platform-wide
		// value, same as the other gap-closed services.
		BatchTimeout: 10 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter, metrics)
	jurisdictionValidator := jurisdiction.NewHTTPValidator(cfg.JurisdictionRulesURL, log)
	authzClient := authz.NewHTTPClient(cfg.AuthZServiceURL, log)

	// ── 4a. Outbox relay ──────────────────────────────────────────────────────
	// Every obligation event is written to event_outbox in the same
	// transaction as the mutation; the relay delivers it to Kafka and retries
	// until the broker accepts it. A broker outage delays events, never loses
	// them.
	outboxWorker := outbox.NewWorker(pgStore, publisher, log).WithGauges(metrics)
	outboxWorker.Start(context.Background())
	defer outboxWorker.Stop()

	// ── 4b. Overdue scheduler ─────────────────────────────────────────────────
	// Runs every minute to find obligations past their due_date and transition
	// them to OVERDUE. This addresses the audit gap where overdue was never
	// computed automatically. The transition writes its events to the outbox;
	// the relay publishes them.
	overdueCtx, overdueCancel := context.WithCancel(context.Background())
	defer overdueCancel()
	go runOverdueScheduler(overdueCtx, pgStore, log)

	// ── 4c. Identity gate and legal-entity registry ──────────────────────────
	// X-Principal-Id / X-Tenant-Id were read exactly as sent, and host port
	// 8088 is published, so anyone who could reach the port could act as any
	// principal in any tenant (Governance audit X5). The gate verifies
	// identity-context-svc's signed envelope (JWKS) and that its session is
	// still live (AU-01), then makes the verified claims the only source of
	// those headers. IDENTITY_VERIFY: require (default outside local) | warn.
	gate, gateSettings, err := idenv.NewGateFromEnv("obligations-svc", log, idenv.EnvOptions{})
	if err != nil {
		log.Fatal("identity gate configuration refused", zap.Error(err))
	}
	log.Info("identity gate", gateSettings.Fields()...)

	var entityChecker registry.Checker = registry.Noop{}
	switch {
	case cfg.TenantRegistryURL != "":
		entityChecker = registry.New(cfg.TenantRegistryURL, log)
	case !cfg.LocalEnv():
		log.Fatal("TENANT_REGISTRY_URL is required outside local/dev: every obligation must name a legal entity that exists in its tenant")
	default:
		log.Warn("TENANT_REGISTRY_URL unset — legal entities are not confirmed against the registry (local/dev only)")
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(idenv.RequestID)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("obligations-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(gate.Middleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, jurisdictionValidator, authzClient, log).WithRegistry(entityChecker)
	handler.RegisterRoutes(r, h)
	handler.RegisterApplicabilityRoutes(r, h)

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

// runOverdueScheduler periodically checks for obligations past their due_date
// and transitions them to OVERDUE status across all tenants. TransitionOverdue
// writes obligation.updated and obligation.overdue to the outbox; the relay
// publishes them, so the sweep itself does not touch Kafka.
func runOverdueScheduler(ctx context.Context, pgStore *store.PgStore, log *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	log.Info("overdue scheduler started")
	for {
		select {
		case <-ctx.Done():
			log.Info("overdue scheduler stopped")
			return
		case <-ticker.C:
			// Get all tenant IDs that have obligations
			tenantIDs, err := pgStore.GetTenantIDsWithObligations(ctx)
			if err != nil {
				log.Error("overdue scheduler: failed to get tenant IDs", zap.Error(err))
				continue
			}

			for _, tenantID := range tenantIDs {
				// Create a tenant-scoped context for each tenant
				tenantCtx := svcmiddleware.WithTenant(ctx, tenantID)

				overdue, err := pgStore.FindOverdueObligations(tenantCtx, tenantID)
				if err != nil {
					log.Error("overdue scheduler: failed to find overdue obligations",
						zap.String("tenant_id", tenantID),
						zap.Error(err))
					continue
				}

				for _, o := range overdue {
					updated, transitioned, err := pgStore.TransitionOverdue(tenantCtx, o.ObligationID)
					if err != nil {
						log.Error("overdue scheduler: failed to transition obligation",
							zap.String("tenant_id", tenantID),
							zap.String("obligation_id", o.ObligationID),
							zap.Error(err))
						continue
					}
					if transitioned {
						log.Info("overdue scheduler: transitioned obligation to OVERDUE",
							zap.String("tenant_id", tenantID),
							zap.String("obligation_id", updated.ObligationID),
							zap.String("due_date", updated.DueDate.Format(time.RFC3339)))
					}
				}
			}
		}
	}
}
