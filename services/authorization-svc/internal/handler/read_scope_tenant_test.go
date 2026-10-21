package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Governance audit N-06: available-actions and /v1/me/capabilities took the
// legal entity from a header or the query and never checked it belonged to
// the caller's tenant, so tenant A's tenant-wide grants were listed as usable
// on tenant B's entity — the defect fixed on /v1/authorize on 12 Oct.
func TestReadRoutes_RefuseAnEntityOfAnotherTenant(t *testing.T) {
	const entity = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	paths := []string{
		"/v1/payments/pay-1/available-actions",
		"/v1/me/capabilities",
	}
	for _, path := range paths {
		for _, tc := range []struct {
			owner string
			want  int
		}{
			{tenantB, http.StatusForbidden}, // another tenant's entity
			{tenantA, http.StatusOK},        // the caller's own
		} {
			s := &stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, entityTenant: tc.owner}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Principal-Id", "p-1")
			req.Header.Set("X-Tenant-Id", tenantA)
			req.Header.Set("X-Legal-Entity-Id", entity)
			w := httptest.NewRecorder()
			newTestRouter(s).ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s, entity owned by %s: want %d, got %d: %s", path, tc.owner[:8], tc.want, w.Code, w.Body.String())
			}
		}
	}
}
