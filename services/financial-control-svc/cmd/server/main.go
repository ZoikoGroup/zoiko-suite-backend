// Package main is the entry point for financial-control-svc (ZS-CONTROL-001).
//
// This service verifies financial truth; it never becomes a second ledger.
// It therefore has NO general-ledger client and no route that can write
// accounting state (Invariant 6) — corrections are referenced, never executed.
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

	"zoiko.io/financial-control-svc/internal/authz"
	"zoiko.io/financial-control-svc/internal/config"
	"zoiko.io/financial-control-svc/internal/engine"
	svcenvelope "zoiko.io/financial-control-svc/internal/envelope"
	"zoiko.io/financial-control-svc/internal/events"
	"zoiko.io/financial-control-svc/internal/handler"
	"zoiko.io/financial-control-svc/internal/health"
	svcmiddleware "zoiko.io/financial-control-svc/internal/middleware"
	"zoiko.io/financial-control-svc/internal/mtls"
	"zoiko.io/financial-control-svc/internal/outbox"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
	"zoiko.io/financial-control-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's constant: this service's mTLS
// identity is infrastructure, not tenant data.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

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

	log.Info("financial-control-svc starting",
		zap.Int("port", cfg.Port), zap.String("db_host", cfg.DB.Host), zap.String("authz_url", cfg.AuthZServiceURL))

	shutdownTracing, err := telemetry.InitTracing(context.Background(), "financial-control-svc", cfg.OTELExporterEndpoint)
	if err != nil {
		log.Fatal("otel tracing init failed", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			log.Error("otel tracer provider shutdown failed", zap.Error(err))
		}
	}()
	metrics := telemetry.NewMetrics("financial-control-svc")

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

	// Kafka is not a fail-fast dependency: control state is durable in Postgres
	// and events are relayed from the outbox whenever the broker is reachable.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter).WithProfile(cfg.EventResidencyRegion, cfg.EventClassification)
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go outbox.NewRelay(pool, publisher, 0, 0, log).Start(relayCtx)

	// Housekeeping: expire runs nobody executed and announce SLA breaches, once each.
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-relayCtx.Done():
				return
			case <-t.C:
				res, err := pgStore.Sweep(relayCtx, time.Now().UTC(), 24*time.Hour, 200)
				if err != nil {
					log.Warn("housekeeping sweep failed", zap.Error(err))
				} else if res.ExpiredRuns+res.SLABreaches+res.Errors > 0 {
					log.Info("housekeeping sweep", zap.Int("expired_runs", res.ExpiredRuns),
						zap.Int("sla_breaches", res.SLABreaches), zap.Int("errors", res.Errors))
				}
			}
		}
	}()

	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		c, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "financial-control-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		authzClient = authz.NewHTTPClientWithHTTPClient(cfg.AuthzMTLSURL, c, log)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("financial-control-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	endpoints, err := source.ParseEndpoints(cfg.SourceEndpoints)
	if err != nil {
		log.Fatal("invalid SOURCE_ENDPOINTS", zap.Error(err))
	}
	log.Info("control source systems registered", zap.Int("count", len(endpoints)))
	executor := engine.New(pgStore, source.NewHTTPFetcher(endpoints, nil), log)

	handler.RegisterRoutes(r, handler.New(pgStore, authzClient, executor, log))

	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      4 * time.Minute, // execute runs the whole pipeline synchronously; see handler.executionTimeout
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", srv.Addr))
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
