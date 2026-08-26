// Tests that GET/PUT /v1/principals/... require X-Tenant-Id and thread it
// through to the store. Before this fix, these four routes took only
// principalID from the URL and never checked any tenant header at all —
// any caller could read or update any tenant's principal, role
// assignments, delegations, or status by principal_id alone.
package context_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// mockAuthzClient is a test stub that always permits.
type mockAuthzClient struct{}

func (m *mockAuthzClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error {
	return nil
}

func newPrincipalsRouter(store *mockPrincipalStore) chi.Router {
	r := chi.NewRouter()
	// Resolver and authenticator are nil: these tests exercise only the
	// /v1/principals routes, which touch neither.
	h := identityctx.NewHandler(nil, nil, nil, store, &mockAuthzClient{}, zap.NewNop())
	identityctx.RegisterRoutes(r, h)
	return r
}

func TestPrincipalRoutes_MissingTenantHeader_Rejected(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1"}}
	r := newPrincipalsRouter(store)

	cases := []struct {
		method, path string
	}{
		{"GET", "/v1/principals/p-1"},
		{"GET", "/v1/principals/p-1/roles"},
		{"GET", "/v1/principals/p-1/delegations"},
		{"PUT", "/v1/principals/p-1/status"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		// Now returns 401 (Unauthorized) instead of 400 for missing tenant
		if w.Code != 401 {
			t.Errorf("%s %s with no X-Tenant-Id: got %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestGetPrincipal_TenantHeaderPresent_ReachesStore(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a"}}
	r := newPrincipalsRouter(store)

	req := httptest.NewRequest("GET", "/v1/principals/p-1", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
