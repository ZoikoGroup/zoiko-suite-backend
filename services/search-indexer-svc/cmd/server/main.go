// search-indexer-svc bootstraps the obligations-to-OpenSearch sync process
// and serves /healthz, /readyz, /metrics, /v1/sync, /v1/status, and /v1/search.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
		PrincipalID:       cfg.principalID,
		TenantID:          cfg.tenantID,
	})

	// HTTP server: health + metrics + management APIs.
	r := chi.NewRouter()
	r.Get("/healthz", health.HandleHealthz)
	r.Get("/readyz", health.HandleReadyz)
	r.Handle("/metrics", promhttp.Handler())

	// Management & QA endpoints
	r.Get("/v1/status", handleStatus(obSyncer, cfg))
	r.Get("/status", handleStatus(obSyncer, cfg))

	r.Post("/v1/sync", handleTriggerSync(obSyncer))
	r.Post("/sync", handleTriggerSync(obSyncer))

	r.Post("/v1/search", handleSearch(sc))
	r.Post("/search", handleSearch(sc))

	r.Post("/v1/index", handleDirectIndex(sc))
	r.Post("/index", handleDirectIndex(sc))

	srv := &http.Server{
		Addr:         ":" + cfg.port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
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

type searchRequest struct {
	TenantID string `json:"tenant_id"`
	Keywords string `json:"keywords"`
	Size     int    `json:"size"`
}

func handleSearch(sc searchclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid_json_body"}`, http.StatusBadRequest)
			return
		}

		if req.TenantID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "tenant_id_required",
				"message": "searchclient: TenantID is required — a search without tenant scope is prohibited",
			})
			return
		}

		results, err := sc.Search(r.Context(), searchclient.IndexObligations, searchclient.SearchQuery{
			TenantID: req.TenantID,
			Keywords: req.Keywords,
			Size:     req.Size,
		})
		if err != nil {
			if errors.Is(err, searchclient.ErrTenantIDRequired) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "tenant_id_required",
					"message": err.Error(),
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "search_failed",
				"message": err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total":     len(results),
			"results":   results,
			"tenant_id": req.TenantID,
			"keywords":  req.Keywords,
		})
	}
}

func handleDirectIndex(sc searchclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var doc searchclient.Document
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			http.Error(w, `{"error":"invalid_json_body"}`, http.StatusBadRequest)
			return
		}

		if doc.ID == "" || doc.TenantID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "missing_required_fields",
				"message": "Both id and tenant_id are strictly required for document indexing",
			})
			return
		}

		if err := sc.Index(r.Context(), searchclient.IndexObligations, doc); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "index_failed",
				"message": err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"indexed":   true,
			"id":        doc.ID,
			"tenant_id": doc.TenantID,
			"index":     searchclient.IndexObligations,
		})
	}
}

func handleTriggerSync(syncer *syncer.ObligationsSyncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		count, err := syncer.RunCycle(r.Context())
		duration := time.Since(start)

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":     false,
				"error":       err.Error(),
				"duration_ms": duration.Milliseconds(),
				"count":       count,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":     true,
			"count":       count,
			"duration_ms": duration.Milliseconds(),
			"synced_at":   time.Now().Format(time.RFC3339),
			"stats":       syncer.GetStats(),
		})
	}
}

func handleStatus(syncer *syncer.ObligationsSyncer, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stats := syncer.GetStats()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":              "search-indexer-svc",
			"port":                 cfg.port,
			"status":               map[string]any{"healthy": true, "ready": stats.IsReady},
			"syncer":               stats,
			"opensearch_addresses": cfg.opensearchAddresses,
			"obligations_svc_url":  cfg.obligationsSvcURL,
			"tenant_svc_url":       cfg.tenantSvcURL,
			"sync_interval":        cfg.syncInterval.String(),
		})
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
	principalID         string
	tenantID            string
}

func loadConfig() (config, error) {
	c := config{
		port:                envOr("PORT", "8096"),
		obligationsSvcURL:   envOr("OBLIGATIONS_SVC_URL", "http://obligations-svc:8088"),
		tenantSvcURL:        envOr("TENANT_SVC_URL", "http://tenant-svc:8081"),
		opensearchAddresses: envOr("OPENSEARCH_ADDRESSES", "http://opensearch:9200"),
		opensearchUsername:  os.Getenv("OPENSEARCH_USERNAME"),
		opensearchPassword:  os.Getenv("OPENSEARCH_PASSWORD"),
		principalID:         envOr("INDEXER_PRINCIPAL_ID", "33333333-3333-3333-3333-333333333333"),
		tenantID:            envOr("INDEXER_TENANT_ID", "11111111-1111-1111-1111-111111111111"),
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
