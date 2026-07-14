// search-indexer-svc bootstraps the obligations-to-OpenSearch sync process
// and serves /healthz, /readyz, and /metrics.
//
// Configuration is entirely through environment variables — no config file.
// See README.md for the full env var reference.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/health"
	syncer "zoiko.io/search-indexer-svc/internal/sync"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal("invalid configuration", zap.Error(err))
	}

	// Build the OpenSearch client.
	sc, err := searchclient.New(searchclient.Config{
		Addresses: strings.Split(cfg.opensearchAddresses, ","),
		Username:  cfg.opensearchUsername,
		Password:  cfg.opensearchPassword,
	})
	if err != nil {
		log.Fatal("failed to create search client", zap.Error(err))
	}

	// Build the syncer.
	obSyncer := syncer.NewObligationsSyncer(syncer.Config{
		ObligationsSvcURL: cfg.obligationsSvcURL,
		TenantSvcURL:      cfg.tenantSvcURL,
		SearchClient:      sc,
		Interval:          cfg.syncInterval,
		Log:               log,
	})

	// HTTP server: health + metrics.
	r := chi.NewRouter()
	r.Get("/healthz", health.HandleHealthz)
	r.Get("/readyz", health.HandleReadyz)
	r.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:         ":" + cfg.port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start sync loop in background.
	go obSyncer.Start(ctx)

	// Start HTTP server.
	go func() {
		log.Info("search-indexer-svc listening", zap.String("port", cfg.port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", zap.Error(err))
	}
}

// config holds all runtime configuration loaded from environment variables.
type config struct {
	port                string
	obligationsSvcURL   string
	tenantSvcURL        string
	opensearchAddresses string
	opensearchUsername  string
	opensearchPassword  string
	syncInterval        time.Duration
}

func loadConfig() (config, error) {
	c := config{
		port:                envOr("PORT", "8094"),
		obligationsSvcURL:   envOr("OBLIGATIONS_SVC_URL", "http://obligations-svc:8088"),
		tenantSvcURL:        envOr("TENANT_SVC_URL", "http://tenant-svc:8081"),
		opensearchAddresses: envOr("OPENSEARCH_ADDRESSES", "http://opensearch:9200"),
		opensearchUsername:  os.Getenv("OPENSEARCH_USERNAME"),
		opensearchPassword:  os.Getenv("OPENSEARCH_PASSWORD"),
	}

	rawInterval := envOr("SYNC_INTERVAL", "60s")
	d, err := time.ParseDuration(rawInterval)
	if err != nil {
		return c, fmt.Errorf("invalid SYNC_INTERVAL %q: %w", rawInterval, err)
	}
	c.syncInterval = d

	if c.obligationsSvcURL == "" {
		return c, fmt.Errorf("OBLIGATIONS_SVC_URL must not be empty")
	}
	if c.opensearchAddresses == "" {
		return c, fmt.Errorf("OPENSEARCH_ADDRESSES must not be empty")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
