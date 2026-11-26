package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tenant-scope tests for forecasting-svc (Priority 1b).
//
// The defect: internal/middleware/tenant.go substituted the literal string
// "default-tenant" when X-Tenant-ID was absent, in TWO places — the
// middleware itself and GetTenantID's fallback — so fixing either alone
// would have left the hole open.
//
// What made it severe here rather than merely wrong is that this service
// has real row-level security, and the fabricated value did not stop at the
// application layer. PgStore pushes it into Postgres:
//
//	SELECT set_config('app.tenant_id', $1, true)
//
// and the policy reads it back:
//
//	USING (tenant_id = current_setting('app.tenant_id', true))
//
// So the policy was never bypassed — it was SATISFIED. Postgres returned
// every row whose tenant_id was 'default-tenant', which is how all
// header-less callers came to share each other's rows legitimately, inside
// the policy. That is worse than no policy: pg_class reports the policy
// exists, and any test passing a real tenant goes green.
//
// These tests prove the HTTP gate; the database half (FORCE RLS + WITH
// CHECK) is proven by internal/store TestRLS_EnforcedByDatabase.

func TestMissingTenantHeader_Refused(t *testing.T) {
	router := setupTestRouter(t)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"Post /generate", http.MethodPost, "/v1/forecasts/generate"},
		{"Get /", http.MethodGet, "/v1/forecasts/"},
		{"Get /some-id", http.MethodGet, "/v1/forecasts/some-id"},
		{"Post /some-id/recalculate", http.MethodPost, "/v1/forecasts/some-id/recalculate"},
		{"Delete /some-id", http.MethodDelete, "/v1/forecasts/some-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Content-Type", "application/json")
			// Deliberately NO X-Tenant-ID.
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-ID, got %d: %s", rec.Code, rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", rec.Body.String())
			}
		})
	}
}

// TestHealthzDoesNotRequireTenant pins a regression I introduced while
// fixing the above and caught here.
//
// The first version applied the tenant middleware globally with r.Use()
// without exempting the probes, so every probe returned 401 and the
// orchestrator would have restarted the container in a loop. The gate is now
// global again (platform middleware order) but exempts /healthz, /readyz and
// /health itself, so unknown paths without a tenant are still 401.
//
// It must assert on a request with NO tenant header: with one, it would pass
// either way and prove nothing.
func TestHealthzDoesNotRequireTenant(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	// Deliberately NO X-Tenant-ID — exactly how a probe arrives.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz must not require a tenant — probes carry none; got %d: %s", rec.Code, rec.Body.String())
	}
}
