package handler_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// Tests for POST /v1/admin/sod-rules/{sod_rule_id}/retire|reactivate.
//
// These routes did not exist. active_flag has been on sod_rules since the
// initial schema and CheckSoDConflict has always filtered on it, so the column
// was the intended off switch — reachable by no route at all. A conflict rule
// could be created and never retired, on the one object whose blast radius is
// every principal holding the pair: an SoD rule authored by mistake denied its
// action tenant-wide with no remedy through the API.
//
// Every sibling object already had a lifecycle: roles retire/reactivate,
// assignments and delegations revoke, abac_rules retire/reactivate. This closes
// the asymmetry on the one that mattered most.

func TestRetireSoDRule_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/rule-1/retire", nil)
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111") // tenant present, principal absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.gotSoDActiveID != "" {
		t.Error("store was reached despite a missing principal — turning off an SoD control must not be unauthenticated")
	}
}

func TestRetireSoDRule_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/rule-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1") // principal present, tenant absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.gotSoDActiveID != "" {
		t.Error("store was reached with no tenant scope — an unscoped update could retire another tenant's control")
	}
}

// The scope must come from the verified header. An SoD rule is a control, and
// a caller able to name the tenant it retires from could disable another
// tenant's segregation of duties.
func TestRetireSoDRule_TenantComesFromHeaderNotQuery(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/admin/sod-rules/rule-1/retire?tenant_id=someone-elses-tenant", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotSoDActiveTenant != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("store scoped to %q; a query param overrode the verified header", store.gotSoDActiveTenant)
	}
}

func TestRetireSoDRule_PassesFalse(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/rule-9/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotSoDActiveID != "rule-9" {
		t.Errorf("rule id reached the store as %q, want rule-9", store.gotSoDActiveID)
	}
	if store.gotSoDActiveValue {
		t.Error("retire passed active=true — that would reactivate the rule, not retire it")
	}
}

func TestReactivateSoDRule_PassesTrue(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/rule-9/reactivate", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !store.gotSoDActiveValue {
		t.Error("reactivate passed active=false")
	}
}

// A platform-wide rule, and one that does not exist, both answer 404 — the
// store's predicate has no IS NULL branch, so a rule binding every tenant is
// absent from any single tenant's scope. That is deliberate: it must not be
// disableable by one of the tenants it binds.
func TestSetSoDRuleActive_NotFoundIs404(t *testing.T) {
	store := &stubStore{setSoDActiveErr: domain.ErrSoDRuleNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/platform-wide/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a rule outside the caller's scope, got %d", w.Code)
	}
}

func TestSetSoDRuleActive_StoreFailureIs503(t *testing.T) {
	store := &stubStore{setSoDActiveErr: errors.New("connection refused")}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules/rule-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 503, not 404: "could not reach the store" and "no such rule" are
	// different facts, and reporting an outage as a missing rule would tell an
	// operator their retirement succeeded against nothing.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a store failure, got %d", w.Code)
	}
}
