package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/hris-connector-svc/internal/authz"
	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/events"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/store"
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

func TestHrisFlow(t *testing.T) {
	r, _ := setupTestRouter()

	integReq := domain.CreateIntegrationRequest{
		LegalEntityID: "le-101",
		ProviderName:  domain.ProviderWorkday,
		ApiEndpoint:   "https://wd5.workday.com/api/v1/zoiko",
	}
	integBytes, _ := json.Marshal(integReq)

	req := httptest.NewRequest("POST", "/v1/hris/integrations", bytes.NewReader(integBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var integ domain.HrisIntegration
	if err := json.Unmarshal(w.Body.Bytes(), &integ); err != nil {
		t.Fatalf("failed to unmarshal integration: %v", err)
	}

	syncReq := domain.TriggerSyncRequest{
		IntegrationID: integ.IntegrationID,
		SyncType:      "EMPLOYEE_ROSTER_SYNC",
	}
	syncBytes, _ := json.Marshal(syncReq)

	syncHTTP := httptest.NewRequest("POST", "/v1/hris/sync", bytes.NewReader(syncBytes))
	syncHTTP.Header.Set("Content-Type", "application/json")
	syncHTTP.Header.Set("X-Tenant-ID", "tenant-test")

	syncW := httptest.NewRecorder()
	r.ServeHTTP(syncW, syncHTTP)

	if syncW.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on trigger sync, got %d: %s", syncW.Code, syncW.Body.String())
	}
}
