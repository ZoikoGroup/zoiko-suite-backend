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

	"zoiko.io/authorization-svc/internal/config"
	"zoiko.io/authorization-svc/internal/events"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/health"
	"zoiko.io/authorization-svc/internal/jurisdiction"
	"zoiko.io/authorization-svc/internal/mtls"
	"zoiko.io/authorization-svc/internal/siem"
	"zoiko.io/authorization-svc/internal/store"
	"zoiko.io/authorization-svc/internal/telemetry"
)

// platformScopeID is the legal_entity_id presented when provisioning this
// service's own mTLS identity — an infrastructure certificate, not scoped
// to any one tenant's data, same convention as AUTHZ_PLATFORM_SCOPE_ID
// elsewhere in this codebase.
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

	siemClient := siem.New(cfg.SIEMServiceURL, "authorization-svc", log)
	h := handler.New(pgStore, publisher, jurisdictionValidator, siemClient, log)
	handler.RegisterRoutes(r, h)

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
			log.Error("mtls listener shutdown failed", zap.Error(err))
		}
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
