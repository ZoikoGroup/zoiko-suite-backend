package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Governance audit N-01 (13 Oct 2026): the admin gates read RBAC grants only,
// so a principal identity-context-svc had suspended kept every IAM
// administration power while /v1/authorize denied them everything. The stub
// grants the caller every admin action; only their standing differs.
func TestAdminGates_RefuseASuspendedPrincipal(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	cases := map[string]string{
		"/v1/admin/roles":                       `{"tenant_id":"` + tenant + `","role_code":"X","role_name":"X","role_scope_type":"ENTITY"}`,
		"/v1/admin/role-assignments":            `{}`,
		"/v1/admin/delegated-authorities":       `{}`,
		"/v1/admin/privileged-sessions":         `{}`,
		"/v1/admin/break-glass-sessions":        `{}`,
		"/v1/admin/sod-exceptions/e-1/approve":  `{}`,
		"/v1/admin/role-assignments/a-1/revoke": `{}`,
	}
	for _, status := range []string{"SUSPENDED", "TERMINATED"} {
		for path, body := range cases {
			store := &stubStore{principalStatusByID: map[string]string{"admin-1": status}}
			r := newTestRouter(store)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("X-Principal-Id", "admin-1")
			req.Header.Set("X-Tenant-Id", tenant)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s by a %s administrator: want 403, got %d: %s", path, status, w.Code, w.Body.String())
				continue
			}
			// Recorded, with the standing as the basis — not "no_grant".
			if got := store.recordedParams.Basis; got != "principal_status:"+status {
				t.Errorf("%s: recorded basis %q, want principal_status:%s", path, got, status)
			}
			if store.recordedParams.Outcome != "DENIED" {
				t.Errorf("%s: recorded outcome %q, want DENIED", path, store.recordedParams.Outcome)
			}
		}
	}
}

// The ACTIVE control: the same caller, not suspended, passes the gate. Without
// it the test above would pass on a gate that refused everybody.
func TestAdminGates_AdmitAnActivePrincipal(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	store := &stubStore{principalStatusByID: map[string]string{"admin-1": "ACTIVE"}}
	r := newTestRouter(store)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles",
		bytes.NewBufferString(`{"tenant_id":"`+tenant+`","role_code":"X","role_name":"X","role_scope_type":"ENTITY"}`))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatalf("an ACTIVE administrator was refused: %s", w.Body.String())
	}
}

// N-01 also covers the read gates and N-13 the artifact: a suspended caller
// listing the decision log gets only the narrow, own-decisions view, and the
// iam.policy.read check that decided it is recorded.
func TestReadGate_SuspendedCallerGetsNarrowViewAndIsRecorded(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	store := &stubStore{principalStatusByID: map[string]string{"admin-1": "SUSPENDED"}}
	r := newTestRouter(store)
	req := httptest.NewRequest(http.MethodGet, "/v1/access-decisions?principal_id=someone-else", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a suspended caller reading another principal's decisions: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(store.recordedParams.Basis, "principal_status:") || store.recordedParams.ActionType != "iam.policy.read" {
		t.Errorf("read-gate check not recorded as a status denial: %+v", store.recordedParams)
	}
}
