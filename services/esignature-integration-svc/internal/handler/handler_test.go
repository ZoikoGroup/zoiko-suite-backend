package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/store"
)

func setupTestRouter() (chi.Router, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	az := authz.NewClient("http://localhost:8081")
	h := New(st, pub, az, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r, pub
}

func TestEsignatureFlow(t *testing.T) {
	r, _ := setupTestRouter()

	// Create envelope
	reqBody := domain.CreateEnvelopeRequest{
		LegalEntityID: "le-101",
		Provider:      domain.ProviderDocuSign,
		DocumentTitle: "Employment Contract — Jane Smith",
		SignerEmail:   "jane.smith@zoiko.com",
		SignerName:    "Jane Smith",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/esignature/envelopes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var env domain.SignatureEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if env.EnvelopeID == "" {
		t.Fatal("expected a non-empty envelope_id")
	}

	// Update status to SIGNED
	updateBody := domain.UpdateStatusRequest{
		Status:      domain.EnvelopeSigned,
		ExternalRef: "DSIGN-ENV-99887",
	}
	updateBytes, _ := json.Marshal(updateBody)

	updateReq := httptest.NewRequest("POST", "/v1/esignature/envelopes/"+env.EnvelopeID+"/status", bytes.NewReader(updateBytes))
	updateReq.Header.Set("Content-Type", "application/json")
	updateReq.Header.Set("X-Tenant-ID", "tenant-test")

	updateW := httptest.NewRecorder()
	r.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected 200 on status update, got %d: %s", updateW.Code, updateW.Body.String())
	}
}
