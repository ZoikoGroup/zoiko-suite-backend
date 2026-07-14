// Package sync provides the obligations syncer for search-indexer-svc.
//
// The syncer polls obligations-svc via its real HTTP API, resolves tenant_id
// for each obligation via tenant-entity-registry-svc, then upserts the record
// into OpenSearch. It never reads obligations-svc's database directly.
//
// Idempotency: every upsert uses obligation_id as the OpenSearch _id, so
// re-running on the same data produces no net change.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
)

// Metrics exposed by the syncer.
var (
	indexedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_indexer_obligations_indexed_total",
		Help: "Total number of obligation records upserted into OpenSearch.",
	}, []string{"status"})

	syncErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "search_indexer_sync_errors_total",
		Help: "Total number of sync cycle errors.",
	})
)

func init() {
	prometheus.MustRegister(indexedTotal, syncErrorsTotal)
}

// obligationResponse is the wire shape returned by GET /v1/obligations.
// Only the fields needed for indexing are mapped here — the indexer is a
// read-only downstream consumer and must not depend on unexported internals
// of obligations-svc.
type obligationResponse struct {
	ObligationID        string    `json:"obligation_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	JurisdictionID      string    `json:"jurisdiction_id"`
	ObligationCode      string    `json:"obligation_code"`
	ObligationType      string    `json:"obligation_type"`
	ObligationStatus    string    `json:"obligation_status"`
	SourceReference     string    `json:"source_reference"`
	ResponsibleFunction string    `json:"responsible_function"`
	SeverityLevel       string    `json:"severity_level"`
	DueDate             time.Time `json:"due_date"`
	CreatedAt           time.Time `json:"created_at"`
}

// Config holds the dependencies and settings for ObligationsSyncer.
type Config struct {
	ObligationsSvcURL string
	TenantSvcURL      string
	SearchClient      searchclient.Client
	Interval          time.Duration
	Log               *zap.Logger
	HTTPClient        *http.Client
}

// ObligationsSyncer polls obligations-svc and upserts records into OpenSearch.
type ObligationsSyncer struct {
	cfg Config

	// tenantCache maps legal_entity_id -> tenant_id to avoid redundant
	// calls to tenant-entity-registry-svc on every sync cycle.
	mu          sync.RWMutex
	tenantCache map[string]string
}

// NewObligationsSyncer constructs and returns a configured syncer.
func NewObligationsSyncer(cfg Config) *ObligationsSyncer {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	return &ObligationsSyncer{
		cfg:         cfg,
		tenantCache: make(map[string]string),
	}
}

// Start begins the sync loop. It runs until ctx is cancelled.
// The first sync runs immediately; subsequent syncs are triggered by the
// configured interval.
func (s *ObligationsSyncer) Start(ctx context.Context) {
	s.cfg.Log.Info("obligations syncer starting",
		zap.String("obligations_svc_url", s.cfg.ObligationsSvcURL),
		zap.Duration("interval", s.cfg.Interval),
	)

	// EnsureIndex once before the first sync.
	if err := s.cfg.SearchClient.EnsureIndex(ctx, searchclient.IndexObligations); err != nil {
		s.cfg.Log.Error("failed to ensure obligations index — sync will retry", zap.Error(err))
	}

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	// Run immediately on start.
	s.runCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			s.cfg.Log.Info("obligations syncer stopped")
			return
		case <-ticker.C:
			s.runCycle(ctx)
		}
	}
}

func (s *ObligationsSyncer) runCycle(ctx context.Context) {
	obligations, err := s.fetchObligations(ctx)
	if err != nil {
		s.cfg.Log.Error("obligations sync: fetch failed", zap.Error(err))
		syncErrorsTotal.Inc()
		return
	}

	s.cfg.Log.Info("obligations sync: fetched records", zap.Int("count", len(obligations)))

	for _, ob := range obligations {
		tenantID, err := s.resolveTenantID(ctx, ob.LegalEntityID)
		if err != nil {
			s.cfg.Log.Warn("obligations sync: skipping record — tenant resolution failed",
				zap.String("obligation_id", ob.ObligationID),
				zap.String("legal_entity_id", ob.LegalEntityID),
				zap.Error(err),
			)
			indexedTotal.WithLabelValues("skip_no_tenant").Inc()
			continue
		}

		doc := searchclient.Document{
			ID:            ob.ObligationID,
			TenantID:      tenantID,
			LegalEntityID: ob.LegalEntityID,
			Body: map[string]any{
				"obligation_id":        ob.ObligationID,
				"obligation_code":      ob.ObligationCode,
				"obligation_type":      ob.ObligationType,
				"obligation_status":    ob.ObligationStatus,
				"source_reference":     ob.SourceReference,
				"responsible_function": ob.ResponsibleFunction,
				"severity_level":       ob.SeverityLevel,
				"jurisdiction_id":      ob.JurisdictionID,
				"due_date":             ob.DueDate,
				"created_at":           ob.CreatedAt,
			},
		}

		if err := s.cfg.SearchClient.Index(ctx, searchclient.IndexObligations, doc); err != nil {
			s.cfg.Log.Error("obligations sync: index failed",
				zap.String("obligation_id", ob.ObligationID),
				zap.Error(err),
			)
			indexedTotal.WithLabelValues("error").Inc()
			continue
		}
		indexedTotal.WithLabelValues("ok").Inc()
	}

	s.cfg.Log.Info("obligations sync: cycle complete", zap.Int("indexed", len(obligations)))
}

// fetchObligations calls GET /v1/obligations on obligations-svc.
func (s *ObligationsSyncer) fetchObligations(ctx context.Context) ([]obligationResponse, error) {
	url := strings.TrimRight(s.cfg.ObligationsSvcURL, "/") + "/v1/obligations"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetchObligations: build request: %w", err)
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchObligations: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("fetchObligations: unexpected status %d: %s", resp.StatusCode, raw)
	}

	var payload []obligationResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("fetchObligations: decode: %w", err)
	}
	return payload, nil
}

// tenantLookupResponse is the minimal shape from tenant-entity-registry-svc.
type tenantLookupResponse struct {
	TenantID string `json:"tenant_id"`
}

// resolveTenantID looks up tenant_id for a legal_entity_id, with an
// in-memory cache to avoid repeated upstream calls.
func (s *ObligationsSyncer) resolveTenantID(ctx context.Context, legalEntityID string) (string, error) {
	s.mu.RLock()
	if tid, ok := s.tenantCache[legalEntityID]; ok {
		s.mu.RUnlock()
		return tid, nil
	}
	s.mu.RUnlock()

	url := fmt.Sprintf("%s/v1/entities/%s", strings.TrimRight(s.cfg.TenantSvcURL, "/"), legalEntityID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("resolveTenantID: build request: %w", err)
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolveTenantID: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolveTenantID: status %d for entity %s", resp.StatusCode, legalEntityID)
	}

	var payload tenantLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("resolveTenantID: decode: %w", err)
	}

	s.mu.Lock()
	s.tenantCache[legalEntityID] = payload.TenantID
	s.mu.Unlock()

	return payload.TenantID, nil
}
