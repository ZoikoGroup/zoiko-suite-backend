package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

func setupTestRouter() (chi.Router, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()

	// Stub authorization-svc that always grants, so tests exercise the
	// real fail-closed CheckAllowed path against a live HTTP contract
	// rather than bypassing it.
	authzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))

	az := authz.NewClient(authzSrv.URL)
	logger := zap.NewNop()

	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r, pub
}

func TestTaxAuthorityFlow(t *testing.T) {
	r, _ := setupTestRouter()

	tfReq := domain.CreateInterfaceRequest{
		LegalEntityID: "le-101",
		Jurisdiction:  "GB",
		AuthorityName: "HMRC MTD UK",
		Protocol:      "REST/OAuth2",
	}
	tfBytes, _ := json.Marshal(tfReq)

	req := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", bytes.NewReader(tfBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")
	req.Header.Set("X-Principal-Id", "user-1")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var tf domain.TaxInterface
	if err := json.Unmarshal(w.Body.Bytes(), &tf); err != nil {
		t.Fatalf("failed to unmarshal interface: %v", err)
	}

	subReq := domain.SubmitTaxFilingRequest{
		InterfaceID: tf.InterfaceID,
		TaxPeriod:   "2026-Q2",
		FilingType:  "VAT_RETURN",
		TaxAmount:   15450.50,
		Payload:     "{\"vat_due\": 15450.50}",
	}
	subBytes, _ := json.Marshal(subReq)

	subHTTP := httptest.NewRequest("POST", "/v1/tax-authority/filings", bytes.NewReader(subBytes))
	subHTTP.Header.Set("Content-Type", "application/json")
	subHTTP.Header.Set("X-Tenant-ID", "tenant-test")
	subHTTP.Header.Set("X-Principal-Id", "user-1")

	subW := httptest.NewRecorder()
	r.ServeHTTP(subW, subHTTP)

	if subW.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on filing submit, got %d: %s", subW.Code, subW.Body.String())
	}
}
