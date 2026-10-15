package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope": the
// admin registers are readable with the Appendix A read permission, and the
// review queue is the verified caller's own.

func getAs(s *stubStore, caller, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if caller != "" {
		req.Header.Set("X-Principal-Id", caller)
		req.Header.Set("X-Tenant-Id", govTenant)
	}
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

func TestReadGates_RegistersNeedReadPermission(t *testing.T) {
	for path, perm := range map[string]string{
		"/v1/admin/roles":                        "iam.role.read",
		"/v1/admin/roles/r-1/permission-bundles": "iam.role.read",
		"/v1/admin/role-assignments":             "iam.assignment.read",
		"/v1/admin/delegated-authorities":        "iam.delegation.read",
		"/v1/admin/sod-rules":                    "iam.sod_rule.read",
		"/v1/admin/abac-rules":                   "iam.policy.read",
		"/v1/support/sessions":                   "iam.support.manage",
		"/v1/admin/break-glass-sessions":         "iam.break_glass.manage",
		"/v1/admin/privileged-sessions":          "iam.pam.manage",
	} {
		if perm == "" {
			continue
		}
		w := getAs(&stubStore{denyAdmin: true}, "clerk-1", path)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), perm) {
			t.Errorf("%s without %s: want 403 naming it, got %d %s", path, perm, w.Code, w.Body.String())
		}
		if w := getAs(&stubStore{denyAdmin: true, rbacActions: []string{perm}}, "auditor-1", path); w.Code != http.StatusOK {
			t.Errorf("%s with %s: want 200, got %d", path, perm, w.Code)
		}
	}
}

// Your own sessions are readable without the administering permission.
func TestReadGates_OwnSessionsReadable(t *testing.T) {
	s := &stubStore{denyAdmin: true, breakGlassSession: &domain.BreakGlassSession{SessionID: "bg-1", PrincipalID: "ops-1"}}
	if w := getAs(s, "ops-1", "/v1/admin/break-glass-sessions?principal_id=ops-1"); w.Code != http.StatusOK {
		t.Errorf("own break-glass sessions: want 200, got %d", w.Code)
	}
	if w := getAs(s, "ops-1", "/v1/admin/break-glass-sessions/bg-1"); w.Code != http.StatusOK {
		t.Errorf("own break-glass session by id: want 200, got %d", w.Code)
	}
	if w := getAs(s, "someone-else", "/v1/admin/break-glass-sessions/bg-1"); w.Code != http.StatusNotFound {
		t.Errorf("another principal's session without iam.break_glass.manage: want 404, got %d", w.Code)
	}
}

// Access reviews belong to access-control-svc (N-03 / N-10). This service's
// copy could not create a review and lost revocations on decide; it must not
// come back unnoticed.
func TestAccessReviewRoutesAreNotServedHere(t *testing.T) {
	s := &stubStore{}
	if w := getAs(s, "reviewer-1", "/v1/iam/access-reviews"); w.Code != http.StatusNotFound {
		t.Errorf("GET /v1/iam/access-reviews: want 404 (owned by access-control-svc), got %d", w.Code)
	}
	for _, path := range []string{"/v1/iam/access-reviews/rv-1/decide", "/v1/iam/access-reviews/rv-1:decide"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"decision":"REVOKE","reason":"x"}`))
		req.Header.Set("X-Principal-Id", "reviewer-1")
		req.Header.Set("X-Tenant-Id", govTenant)
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: want 404/405 (owned by access-control-svc), got %d", path, w.Code)
		}
	}
}
