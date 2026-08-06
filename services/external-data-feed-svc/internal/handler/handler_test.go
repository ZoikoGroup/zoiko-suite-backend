package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/external-data-feed-svc/internal/authz"
	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/events"
	"zoiko.io/external-data-feed-svc/internal/middleware"
	"zoiko.io/external-data-feed-svc/internal/store"
)

// newGrantingAuthzServer stands in for authorization-svc in tests: it
// always grants, matching the real service's contract of always returning
// HTTP 200 with a decision in the body.
func newGrantingAuthzServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupTestRouter(t *testing.T) (chi.Router, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	authzSrv := newGrantingAuthzServer(t)
	az := authz.NewClient(authzSrv.URL)
	h := New(st, pub, az, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r, pub
}

func TestExternalDataFeedFlow(t *testing.T) {
	r, _ := setupTestRouter(t)

	// Create subscription
	subReq := domain.CreateSubscriptionRequest{
		LegalEntityID: "le-200",
		Provider:      "Bloomberg",
		FeedType:      domain.FeedTypeFXRate,
		Symbol:        "GBP/USD",
	}
	body, _ := json.Marshal(subReq)

	req := httptest.NewRequest("POST", "/v1/external-data-feeds/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var sub domain.DataFeedSubscription
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if sub.FeedID == "" {
		t.Fatal("expected non-empty feed_id")
	}

	// Ingest event against the subscription
	ingestReq := domain.IngestEventRequest{
		FeedID:    sub.FeedID,
		EventType: "fx.rate.update",
		Payload:   map[string]interface{}{"pair": "GBP/USD", "rate": 1.2745, "timestamp": "2026-07-23T06:00:00Z"},
	}
	ingestBody, _ := json.Marshal(ingestReq)

	ingestHTTP := httptest.NewRequest("POST", "/v1/external-data-feeds/events/ingest", bytes.NewReader(ingestBody))
	ingestHTTP.Header.Set("Content-Type", "application/json")
	ingestHTTP.Header.Set("X-Tenant-ID", "tenant-test")
	ingestHTTP.Header.Set("X-Principal-Id", "principal-01")

	ingestW := httptest.NewRecorder()
	r.ServeHTTP(ingestW, ingestHTTP)

	if ingestW.Code != http.StatusCreated {
		t.Fatalf("expected 201 on ingest, got %d: %s", ingestW.Code, ingestW.Body.String())
	}
}
