package sync_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"zoiko.io/search-client/searchclient"
	syncer "zoiko.io/search-indexer-svc/internal/sync"
)

type mockSearchClient struct {
	indexedCount int32
}

func (m *mockSearchClient) EnsureIndex(ctx context.Context, index searchclient.IndexName) error {
	return nil
}

func (m *mockSearchClient) Index(ctx context.Context, index searchclient.IndexName, doc searchclient.Document) error {
	atomic.AddInt32(&m.indexedCount, 1)
	return nil
}

func (m *mockSearchClient) Search(ctx context.Context, index searchclient.IndexName, q searchclient.SearchQuery) ([]searchclient.SearchResult, error) {
	return nil, nil
}

func TestObligationsSyncer_HeadersAndIndex(t *testing.T) {
	var receivedTenantHeader string
	var receivedPrincipalHeader string

	// Mock obligations-svc server
	obServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTenantHeader = r.Header.Get("X-Tenant-Id")
		receivedPrincipalHeader = r.Header.Get("X-Principal-Id")

		obligations := []map[string]interface{}{
			{
				"obligation_id":        "ob-1",
				"legal_entity_id":      "ent-1",
				"jurisdiction_id":      "jur-1",
				"obligation_code":      "CODE-1",
				"obligation_type":      "STATUTORY",
				"obligation_status":    "ACTIVE",
				"source_reference":     "REF-1",
				"responsible_function": "TAX",
				"severity_level":       "HIGH",
				"due_date":             time.Now().UTC().Format(time.RFC3339),
				"created_at":           time.Now().UTC().Format(time.RFC3339),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obligations)
	}))
	defer obServer.Close()

	// Mock tenant-entity-registry-svc server
	tenantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tenant_id": "tenant-resolved",
		})
	}))
	defer tenantServer.Close()

	mockSC := &mockSearchClient{}
	s := syncer.NewObligationsSyncer(syncer.Config{
		ObligationsSvcURL: obServer.URL,
		TenantSvcURL:      tenantServer.URL,
		TenantIDs:         []string{"tenant-test"},
		SearchClient:      mockSC,
		Interval:          time.Millisecond * 10,
		Log:               zaptest.NewLogger(t),
	})

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Wait for first cycle
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&mockSC.indexedCount) > 0
	}, 2*time.Second, 50*time.Millisecond)

	cancel()

	assert.Equal(t, "tenant-test", receivedTenantHeader)
	assert.Equal(t, "search-indexer-svc", receivedPrincipalHeader)
}
