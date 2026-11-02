package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/hris-connector-svc/internal/authz"
	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/events"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/provider"
	"zoiko.io/hris-connector-svc/internal/store"
)

type fixedAdapter struct {
	records int
	err     error
}

func (f fixedAdapter) Sync(context.Context, provider.SyncRequest) (provider.SyncResult, error) {
	return provider.SyncResult{RecordsSynced: f.records}, f.err
}

func routerWith(t *testing.T, authzURL string, reg *provider.Registry) chi.Router {
	t.Helper()
	h := New(store.NewMemoryStore(), events.NewMockPublisher(), authz.NewClient(authzURL), zap.NewNop())
	if reg != nil {
		h.WithProviders(reg)
	}
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r
}

// newPolicyAuthzServer grants exactly the (principal, action) pairs listed
// and denies everything else, answering HTTP 200 like authorization-svc.
func newPolicyAuthzServer(t *testing.T, grants map[string]bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PrincipalID string `json:"principal_id"`
			ActionType  string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		outcome := "DENIED"
		if grants[req.PrincipalID+"|"+req.ActionType] {
			outcome = "GRANTED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func do(r chi.Router, method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createIntegration(t *testing.T, r chi.Router, tenant, principal string) string {
	t.Helper()
	w := do(r, http.MethodPost, "/v1/hris/integrations", tenant, principal,
		map[string]string{"legal_entity_id": "le-1", "provider_name": domain.ProviderWorkday, "api_endpoint": "https://wd.example/api"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create integration: %d %s", w.Code, w.Body.String())
	}
	var integ domain.HrisIntegration
	_ = json.Unmarshal(w.Body.Bytes(), &integ)
	if integ.Status != domain.SyncPending {
		t.Fatalf("a newly registered integration has synced nothing; status = %q, want PENDING", integ.Status)
	}
	return integ.IntegrationID
}

// Regression: TriggerSync used to persist COMPLETED/records_synced=1 with no
// provider. With no adapter configured it must refuse and record nothing.
func TestTriggerSync_ProviderNotConfigured_503AndNothingRecorded(t *testing.T) {
	r := routerWith(t, newGrantingAuthzServer(t).URL, nil)
	id := createIntegration(t, r, "tenant-a", "p1")

	w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-a", "p1", map[string]string{"integration_id": id, "sync_type": "EMPLOYEES"})
	if w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte("provider_not_configured")) {
		t.Fatalf("got %d %s, want 503 provider_not_configured", w.Code, w.Body.String())
	}
	list := do(r, http.MethodGet, "/v1/hris/sync/jobs?integration_id="+id, "tenant-a", "p1", nil)
	if !bytes.Contains(list.Body.Bytes(), []byte(`"total":0`)) {
		t.Fatalf("no sync job may be recorded when no provider ran: %s", list.Body.String())
	}
}

func TestTriggerSync_ProviderFailure_RecordedAsFailed502(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(domain.ProviderWorkday, fixedAdapter{err: errors.New("upstream 500 secret-detail")})
	r := routerWith(t, newGrantingAuthzServer(t).URL, reg)
	id := createIntegration(t, r, "tenant-a", "p1")

	w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-a", "p1", map[string]string{"integration_id": id, "sync_type": "EMPLOYEES"})
	if w.Code != http.StatusBadGateway || !bytes.Contains(w.Body.Bytes(), []byte(domain.SyncFailed)) {
		t.Fatalf("got %d %s, want 502 with a FAILED job", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("secret-detail")) {
		t.Fatalf("provider error detail must not be echoed: %s", w.Body.String())
	}
}

func TestAuthz_AllowedAndDenied(t *testing.T) {
	srv := newPolicyAuthzServer(t, map[string]bool{
		"admin|" + HRIS_INTEGRATION_CREATE: true,
		"admin|" + HRIS_SYNC_TRIGGER:       true,
	})
	reg := provider.NewRegistry()
	reg.Register(domain.ProviderWorkday, fixedAdapter{records: 3})
	r := routerWith(t, srv.URL, reg)

	body := map[string]string{"legal_entity_id": "le-1", "provider_name": domain.ProviderWorkday, "api_endpoint": "https://wd.example/api"}
	if w := do(r, http.MethodPost, "/v1/hris/integrations", "tenant-a", "viewer", body); w.Code != http.StatusForbidden {
		t.Fatalf("denied create: got %d, want 403", w.Code)
	}
	id := createIntegration(t, r, "tenant-a", "admin")
	syncBody := map[string]string{"integration_id": id, "sync_type": "EMPLOYEES"}
	if w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-a", "viewer", syncBody); w.Code != http.StatusForbidden {
		t.Fatalf("denied sync: got %d, want 403", w.Code)
	}
	if w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-a", "admin", syncBody); w.Code != http.StatusCreated {
		t.Fatalf("granted sync: got %d %s, want 201", w.Code, w.Body.String())
	}
	if w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-a", "", syncBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: got %d, want 401", w.Code)
	}
}

func TestCrossTenant_IntegrationIsInvisible(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(domain.ProviderWorkday, fixedAdapter{records: 3})
	r := routerWith(t, newGrantingAuthzServer(t).URL, reg)
	id := createIntegration(t, r, "tenant-a", "p1")

	if w := do(r, http.MethodGet, "/v1/hris/integrations/"+id, "tenant-b", "p1", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read: got %d, want 404", w.Code)
	}
	if w := do(r, http.MethodPost, "/v1/hris/sync", "tenant-b", "p1", map[string]string{"integration_id": id, "sync_type": "EMPLOYEES"}); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant sync: got %d, want 404", w.Code)
	}
}

func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(srv.Close)
	r := routerWith(t, srv.URL, nil)
	w := do(r, http.MethodPost, "/v1/hris/integrations", "tenant-a", "p1",
		map[string]string{"legal_entity_id": "le-1", "provider_name": domain.ProviderWorkday})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz down: got %d, want 503", w.Code)
	}
}
