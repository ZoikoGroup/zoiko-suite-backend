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

// The review queue and review decisions take the reviewer from the verified
// header only — a principal_id query parameter was enough to act as anyone.
func TestReadGates_AccessReviewsNoImpersonation(t *testing.T) {
	s := &stubStore{}
	w := getAs(s, "", "/v1/iam/access-reviews?principal_id=reviewer-1&tenant_id="+govTenant)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("review list with identity only in the query: want 401, got %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/iam/access-reviews/rv-1/decide?principal_id=reviewer-1&tenant_id="+govTenant,
		strings.NewReader(`{"decision":"KEEP","reason":"ok"}`))
	w = httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("deciding a review as someone else via the query: want 401, got %d", w.Code)
	}
}
