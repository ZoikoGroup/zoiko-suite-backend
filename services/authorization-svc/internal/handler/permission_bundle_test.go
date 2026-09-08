package handler_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// Tests for the permission-bundle read and the permission-bundle off switch —
// GET /v1/admin/roles/{role_id}/permission-bundles and
// POST /v1/admin/permission-bundles/{id}/retire|reactivate — plus the
// created-vs-replaced distinction on the POST that authors them.
//
// WHY THESE THREE THINGS BELONG IN ONE FILE. They are one defect seen from
// three sides. `permitted_actions` is what a role actually grants, and it was
// write-only: nothing could list it (ListRoles returns no actions), nothing
// could switch one bundle off (`pb.active_flag` is in the JOIN of BOTH
// evaluation reads with no writer anywhere), and the upsert that authors them
// replaces an existing action set wholesale while answering 201 as though it
// had created something. The three compose into the case that matters: an
// operator could not see what a role permitted, could not withdraw part of
// it, and could destroy the rest without being told.
//
// The guards are asserted first, for the reason list_catalogue_test.go states:
// a role's permitted actions are the who-can-do-what map, and an unscoped read
// here hands over every tenant's copy of it.

// ── GET /v1/admin/roles/{role_id}/permission-bundles ────────────────────────

func TestListPermissionBundles_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-1/permission-bundles", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1") // tenant present, principal absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.bundlesTenantID != "" {
		t.Error("store was reached despite a missing principal — the guard runs after the read")
	}
}

func TestListPermissionBundles_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-1/permission-bundles", nil)
	req.Header.Set("X-Principal-Id", "admin-1") // principal present, tenant absent
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.bundlesTenantID != "" {
		t.Error("store was reached with no tenant scope — an unscoped read would expose another tenant's granted actions")
	}
}

// The scope comes from the verified header, and the role from the URL path.
// A caller naming another tenant's role_id gets that tenant's scope applied to
// it, which is what makes the store's EXISTS predicate return nothing.
func TestListPermissionBundles_ScopeFromHeader_RoleFromPath(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-9/permission-bundles?tenant_id=someone-elses-tenant", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.bundlesTenantID != "tenant-1" {
		t.Errorf("store scoped to %q; a query param overrode the verified header", store.bundlesTenantID)
	}
	if store.bundlesRoleID != "r-9" {
		t.Errorf("store asked for role %q, want r-9", store.bundlesRoleID)
	}
}

// Retired bundles are returned. A retired bundle is why an action a role used
// to grant is gone, so hiding it makes that unexplainable — the same reason
// ListRoles keeps retired roles. There is no active_only parameter here at
// all: the store always returns both and orders retired last.
func TestListPermissionBundles_IncludesRetired(t *testing.T) {
	store := &stubStore{
		bundles: []domain.PermissionBundle{
			{PermissionBundleID: "b-1", BundleCode: "live", ActiveFlag: true, PermittedActions: []string{"PAYMENT_APPROVE"}},
			{PermissionBundleID: "b-2", BundleCode: "retired", ActiveFlag: false, PermittedActions: []string{"PAYMENT_INITIATE"}},
		},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-1/permission-bundles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got []domain.PermissionBundle
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both bundles, got %d — a retired bundle is why an action is gone", len(got))
	}
	// The actions are the point of the endpoint: a bundle without them is a
	// label, which is exactly what ListRoles already returned.
	if len(got[0].PermittedActions) == 0 {
		t.Error("permitted_actions came back empty — the read exists to expose them")
	}
}

// A role that does not exist in this scope is 200-with-[], not 404: the
// sibling catalogue reads take the same posture, and 404 here would make the
// endpoint an existence oracle for other tenants' role ids.
func TestListPermissionBundles_EmptyIsArrayNotNull(t *testing.T) {
	store := &stubStore{bundles: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-unknown/permission-bundles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an unknown role, got %d", w.Code)
	}
	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("[]")) {
		t.Errorf("expected [] for an empty list, got %q — null breaks a client that iterates", body)
	}
}

func TestListPermissionBundles_StoreFailureIs503(t *testing.T) {
	store := &stubStore{bundlesErr: errors.New("boom")}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles/r-1/permission-bundles", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a store failure, got %d", w.Code)
	}
}

// ── POST /v1/admin/permission-bundles/{id}/retire|reactivate ────────────────

func TestRetirePermissionBundle_RequiresPrincipal(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/permission-bundles/b-1/retire", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w.Code)
	}
	if store.bundleActiveCalls != 0 {
		t.Error("store was reached despite a missing principal — withdrawing a grant needs an actor")
	}
}

func TestRetirePermissionBundle_RequiresTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/permission-bundles/b-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-Id, got %d", w.Code)
	}
	if store.bundleActiveCalls != 0 {
		t.Error("store was reached with no tenant scope — an unscoped flip could withdraw another tenant's grants")
	}
}

// The two routes differ in exactly one thing: the boolean they pass down.
// Asserted because the retire/reactivate pair is easy to wire the wrong way
// round, and the failure mode is a button that grants where it says withdraw.
func TestPermissionBundleLifecycle_RoutesPassTheRightFlag(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/v1/admin/permission-bundles/b-1/retire", false},
		{"/v1/admin/permission-bundles/b-1/reactivate", true},
	} {
		store := &stubStore{bundleActive: &domain.PermissionBundle{PermissionBundleID: "b-1", BundleCode: "default"}}
		r := newTestRouter(store)

		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", "tenant-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", tc.path, w.Code, w.Body.String())
		}
		if len(store.bundleActiveWant) != 1 || store.bundleActiveWant[0] != tc.want {
			t.Errorf("%s: store received active=%v, want %v", tc.path, store.bundleActiveWant, tc.want)
		}
		if store.bundleActiveTenant != "tenant-1" {
			t.Errorf("%s: store scoped to %q, want the verified header", tc.path, store.bundleActiveTenant)
		}
		if store.bundleActiveID != "b-1" {
			t.Errorf("%s: store asked for bundle %q, want b-1", tc.path, store.bundleActiveID)
		}
	}
}

// 404, not 403 — a probe against another tenant's bundle id must not be able
// to confirm it exists. Same posture the sibling lifecycle routes take.
func TestRetirePermissionBundle_NotFoundIs404(t *testing.T) {
	store := &stubStore{bundleActiveErr: domain.ErrPermissionBundleNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/permission-bundles/b-elsewhere/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a bundle outside this tenant, got %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "permission_bundle_not_found" {
		t.Errorf("error code %q, want permission_bundle_not_found", body["error"])
	}
}

func TestRetirePermissionBundle_StoreFailureIs503(t *testing.T) {
	store := &stubStore{bundleActiveErr: errors.New("boom")}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/permission-bundles/b-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on a store failure, got %d", w.Code)
	}
}

// ── created vs replaced on POST .../permission-bundles ──────────────────────

// The distinction the response used to hide. The upsert on
// (role_id, bundle_code) overwrites permitted_actions wholesale, so a repost
// can silently narrow or empty what a role grants — and it answered 201, the
// same as a genuine creation. 200 now means "an existing bundle's actions were
// replaced", which is a destructive edit the caller is entitled to know about.
func TestCreatePermissionBundle_ReplacedAnswers200NotCreated(t *testing.T) {
	store := &stubStore{
		role:          &domain.Role{RoleID: "r-1", TenantID: "tenant-1"},
		bundle:        &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1", BundleCode: "default"},
		bundleCreated: false, // the DO UPDATE path
	}
	r := newTestRouter(store)

	body := `{"bundle_code":"default","permitted_actions":["PAYMENT_APPROVE"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/permission-bundles", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when an existing bundle_code was replaced, got %d: %s", w.Code, w.Body.String())
	}
}
