package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// Tests for the two catalogue reads: GET /v1/admin/roles and
// GET /v1/admin/delegated-authorities.
//
// Both surfaces were write-only. A role could be created, retired,
// reactivated and given permission bundles, and a delegation could be created
// and revoked, and neither could be listed back — so the only way to learn an
// id was to have been the caller that wrote it, and neither register could be
// audited from outside that caller.
//
// The guards are asserted first, for the same reason list_admin_test.go
// asserts them: a role catalogue names what a tenant has defined and a
// delegation register names who is acting on whose behalf, which is precisely
// the who-can-do-what map GetAccessDecision was hardened to stop leaking. An
// unscoped list here would hand over every tenant's copy of it.

// ── GET /v1/admin/roles ─────────────────────────────────────────────────────

func TestListRoles_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1") // tenant present, principal absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.gotRolesTenant != "" {
		t.Error("store was reached despite a missing principal — the guard runs after the read")
	}
}

func TestListRoles_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Principal-Id", "admin-1") // principal present, tenant absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.gotRolesTenant != "" {
		t.Error("store was reached with no tenant scope — an unscoped list would return every tenant's roles")
	}
}

// The scope must come from the verified header, never from a query parameter a
// caller can set. Same defect class as the admin writes that once took
// tenant_id from the request body.
func TestListRoles_TenantComesFromHeaderNotQuery(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles?tenant_id=someone-elses-tenant", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotRolesTenant != "tenant-1" {
		t.Errorf("store scoped to %q; a query param overrode the verified header", store.gotRolesTenant)
	}
}

// Retired roles are included unless the caller asks otherwise. A retired role
// is the reason access somebody used to hold is gone, so defaulting to hiding
// it would make that unexplainable from the console.
func TestListRoles_IncludesRetiredByDefault(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotRolesActiveOnly {
		t.Error("activeOnly was true with no active_only parameter — retired roles would be hidden by default")
	}
}

func TestListRoles_ActiveOnlyIsOptIn(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles?active_only=true", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !store.gotRolesActiveOnly {
		t.Error("active_only=true did not reach the store")
	}
}

// An empty catalogue is `[]`, never `null`. A JSON null decodes to a nil slice
// in some clients and throws in others, and neither is what "this tenant has
// defined no roles" should look like.
func TestListRoles_EmptyIsArrayNotNull(t *testing.T) {
	store := &stubStore{roles: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var decoded []domain.Role
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body did not decode as an array: %v (body %q)", err, w.Body.String())
	}
	if got := w.Body.String(); got == "null\n" {
		t.Error("empty catalogue serialised as null rather than []")
	}
}

// A store failure is 503 with nothing invented. "Could not read the catalogue"
// and "this tenant has no roles" are opposite answers and an empty 200 would
// conflate them.
func TestListRoles_StoreFailureIs503(t *testing.T) {
	store := &stubStore{listRolesErr: errors.New("connection refused")}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a store failure, got %d", w.Code)
	}
}

// ── GET /v1/admin/delegated-authorities ─────────────────────────────────────

func TestListDelegatedAuthorities_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/delegated-authorities", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.gotDelegTenant != "" {
		t.Error("store was reached despite a missing principal")
	}
}

func TestListDelegatedAuthorities_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/delegated-authorities", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.gotDelegTenant != "" {
		t.Error("store was reached with no tenant scope — an unscoped list would expose every tenant's delegations")
	}
}

func TestListDelegatedAuthorities_TenantComesFromHeaderNotQuery(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/admin/delegated-authorities?tenant_id=someone-elses-tenant", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.gotDelegTenant != "tenant-1" {
		t.Errorf("store scoped to %q; a query param overrode the verified header", store.gotDelegTenant)
	}
}

func TestListDelegatedAuthorities_ForwardsPrincipalFilter(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/admin/delegated-authorities?principal_id=p-9&active_only=true", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if store.gotDelegPrincipal != "p-9" {
		t.Errorf("principal filter reached the store as %q, want p-9", store.gotDelegPrincipal)
	}
	if !store.gotDelegActiveOnly {
		t.Error("active_only=true did not reach the store")
	}
}

// Revoked and expired delegations are returned unless the caller narrows the
// list. A revoked delegation is the evidence that borrowed authority was
// withdrawn, which is what an auditor is looking for.
func TestListDelegatedAuthorities_IncludesRevokedByDefault(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/delegated-authorities", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if store.gotDelegActiveOnly {
		t.Error("activeOnly was true with no active_only parameter — revoked delegations would be hidden by default")
	}
}

func TestListDelegatedAuthorities_EmptyIsArrayNotNull(t *testing.T) {
	store := &stubStore{delegations: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/delegated-authorities", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var decoded []domain.DelegatedAuthority
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body did not decode as an array: %v (body %q)", err, w.Body.String())
	}
	if got := w.Body.String(); got == "null\n" {
		t.Error("empty register serialised as null rather than []")
	}
}

func TestListDelegatedAuthorities_StoreFailureIs503(t *testing.T) {
	store := &stubStore{listDelegationsErr: errors.New("connection refused")}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/delegated-authorities", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a store failure, got %d", w.Code)
	}
}
