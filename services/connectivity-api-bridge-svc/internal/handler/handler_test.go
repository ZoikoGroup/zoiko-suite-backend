package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/events"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
)

func setupTestRouter() (chi.Router, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	az := authz.NewClient("http://localhost:8081")
	logger := zap.NewNop()

	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r, pub
}

func TestCreateAndIngestBridge(t *testing.T) {
	r, _ := setupTestRouter()

	reqBody := domain.CreateBridgeRequest{
		LegalEntityID: "le-101",
		BridgeName:    "Partner ERP Bridge",
		Protocol:      "REST/JSON",
		EndpointURL:   "https://api.partner.com/v1/feed",
		AuthType:      "OAuth2",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/bridges", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var created domain.ApiBridge
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to unmarshal bridge: %v", err)
	}

	if created.BridgeID == "" || created.BridgeName != "Partner ERP Bridge" {
		t.Fatalf("invalid bridge response: %+v", created)
	}

	// Ingest payload test
	ingestBody := domain.IngestPayloadRequest{
		PayloadSummary: "Batch Invoice Ingestion #994",
		RawData:        "{\"invoices\": [101, 102]}",
	}
	ingestBytes, _ := json.Marshal(ingestBody)

	ingestReq := httptest.NewRequest("POST", "/v1/bridges/"+created.BridgeID+"/ingest", bytes.NewReader(ingestBytes))
	ingestReq.Header.Set("Content-Type", "application/json")
	ingestReq.Header.Set("X-Tenant-ID", "tenant-test")

	ingestW := httptest.NewRecorder()
	r.ServeHTTP(ingestW, ingestReq)

	if ingestW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on ingest, got %d: %s", ingestW.Code, ingestW.Body.String())
	}
}

func TestListBridges(t *testing.T) {
	r, _ := setupTestRouter()

	req := httptest.NewRequest("GET", "/v1/bridges?legal_entity_id=le-101", nil)
	req.Header.Set("X-Tenant-ID", "tenant-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
}
