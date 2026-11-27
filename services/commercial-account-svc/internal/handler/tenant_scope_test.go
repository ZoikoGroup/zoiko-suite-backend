package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// Tenant-scope tests for commercial-account-svc (tracker row 11a).
//
// This service's reads were exposed by two distinct defects that compose:
//
//  1. middleware.TenantContext lets a request with no X-Tenant-Id through
//     (it must — the price-catalog and plan routes are platform-scope and
//     legitimately carry no tenant), and
//  2. the store wrote its tenant predicate as ($2 = '' OR
//     organization_id::text = $2), where $2 is the tenant from context.
//
// So a header-less request disabled its own tenant predicate — the GET
// account route has no authz check to fall back on, so that was an open
// read of any account by id.
//
// The membership-specific cases this file used to cover (GET/DELETE
// /v1/memberships/{id}, GET /v1/organizations/{id}/memberships) are retired
// along with the rest of the /v1/memberships surface — see
// SPEC_DEVIATIONS.md A3 and handler.go. Their defects were fixed before
// retirement and are not being un-fixed; the routes exercising the fix no
// longer exist.

func newScopeRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)
	return r
}

// TestMissingTenantHeader_Refused covers defect 1: a request with no
// X-Tenant-Id used to reach the store, where the tenant predicate then
// disabled itself.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	for _, tc := range []struct {
		name, method, path string
	}{
		{"get commercial account", http.MethodGet, "/v1/commercial-accounts/ca-test-001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			// Deliberately NO X-Tenant-Id.
			req.Header.Set("X-Principal-Id", "principal-test-01")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
