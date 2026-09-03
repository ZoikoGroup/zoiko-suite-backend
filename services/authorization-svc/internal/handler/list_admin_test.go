package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// Tests for the two read endpoints added alongside migration 000007:
// GET /v1/admin/role-assignments and GET /v1/admin/sod-rules.
//
// Both existed only as writes before. The gap mattered because the console
// could create a grant and then never show it, and revoking one needs an
// assignment_id that nothing surfaced — so "revoke" was unreachable from any
// UI. These assert the guards first (an assignment names a principal and the
// role they hold, which is the who-can-do-what map GetAccessDecision was
// hardened to stop leaking) and the filter plumbing second.

// ── GET /v1/admin/role-assignments ──────────────────────────────────────────

func TestListRoleAssignments_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1") // tenant present, principal absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.gotAssignTenant != "" {
		t.Error("store was reached despite a missing principal — the guard runs after the read")
	}
}

func TestListRoleAssignments_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Principal-Id", "admin-1") // principal present, tenant absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.gotAssignTenant != "" {
		t.Error("store was reached with no tenant scope — an unscoped list would return every tenant's grants")
	}
}

// The tenant the store is scoped to must come from the verified header, never
// from a query param a caller can set. Same defect class as the one fixed on
// the admin write routes, where tenant_id came from the request body.
func TestListRoleAssignments_TenantComesFromHeaderNotQuery(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments?tenant_id=someone-elses-tenant", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotAssignTenant != "tenant-1" {
		t.Errorf("store scoped to %q; a query param overrode the verified header", store.gotAssignTenant)
	}
}

func TestListRoleAssignments_ForwardsFilters(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/admin/role-assignments?principal_id=p-9&role_id=r-7", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotAssignPrincipal != "p-9" {
		t.Errorf("principal filter not forwarded: got %q", store.gotAssignPrincipal)
	}
	if store.gotAssignRole != "r-7" {
		t.Errorf("role filter not forwarded: got %q", store.gotAssignRole)
	}
}

// Default is active-only, because the list exists to support a revoke
// decision and a revoked row is not revocable. include_expired=true opts into
// the history.
func TestListRoleAssignments_ActiveOnlyByDefault(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if !store.gotAssignActiveOnly {
		t.Error("expected activeOnly=true by default")
	}

	store2 := &stubStore{}
	r2 := newTestRouter(store2)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments?include_expired=true", nil)
	req2.Header.Set("X-Principal-Id", "admin-1")
	req2.Header.Set("X-Tenant-Id", "tenant-1")
	r2.ServeHTTP(httptest.NewRecorder(), req2)

	if store2.gotAssignActiveOnly {
		t.Error("include_expired=true should set activeOnly=false")
	}
}

// An empty result must be [] and not null: the console renders the response
// directly, and `null` is not iterable in the browser.
func TestListRoleAssignments_EmptyIsArrayNotNull(t *testing.T) {
	store := &stubStore{listAssignments: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var decoded []domain.PrincipalRoleAssignment
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not a JSON array: %v (body %s)", err, w.Body.String())
	}
	if got := w.Body.String(); got == "null" || got == "null\n" {
		t.Errorf("empty list serialised as null, not []: %q", got)
	}
}

func TestListRoleAssignments_ReturnsRows(t *testing.T) {
	now := time.Now().UTC()
	store := &stubStore{listAssignments: []domain.PrincipalRoleAssignment{
		{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1", EffectiveFrom: now},
	}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var decoded []domain.PrincipalRoleAssignment
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 || decoded[0].PrincipalRoleAssignmentID != "a-1" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// A store failure must be 503, distinct from "evaluated and found nothing" —
// the same fail-closed posture the authorize path takes.
func TestListRoleAssignments_StoreUnavailableIs503(t *testing.T) {
	store := &stubStore{listAssignmentsErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/role-assignments", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/admin/sod-rules ─────────────────────────────────────────────────

func TestListSoDRules_RequiresPrincipalAndTenant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"no principal", map[string]string{"X-Tenant-Id": "tenant-1"}},
		{"no tenant", map[string]string{"X-Principal-Id": "admin-1"}},
		{"neither", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{}
			r := newTestRouter(store)

			req := httptest.NewRequest(http.MethodGet, "/v1/admin/sod-rules", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
			if store.gotSoDTenant != "" {
				t.Error("store was reached before the guards passed")
			}
		})
	}
}

func TestListSoDRules_ScopedToVerifiedTenant(t *testing.T) {
	store := &stubStore{listSoDRules: []domain.SoDRule{
		{SoDRuleID: "s-1", DomainCode: "PAYMENTS", ActionA: "PAYMENT_APPROVE", ActionB: "PAYMENT_INITIATE", ActiveFlag: true},
	}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sod-rules", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotSoDTenant != "tenant-1" {
		t.Errorf("store scoped to %q, want tenant-1", store.gotSoDTenant)
	}
	var decoded []domain.SoDRule
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 || decoded[0].ActionB != "PAYMENT_INITIATE" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestListSoDRules_EmptyIsArrayNotNull(t *testing.T) {
	store := &stubStore{listSoDRules: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sod-rules", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Body.String(); got == "null" || got == "null\n" {
		t.Errorf("empty list serialised as null, not []: %q", got)
	}
}

func TestListSoDRules_StoreUnavailableIs503(t *testing.T) {
	store := &stubStore{listSoDRulesErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sod-rules", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
