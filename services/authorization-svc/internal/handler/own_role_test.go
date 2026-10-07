package handler_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// A tenant administrator may not widen a role they hold. CreateRoleAssignment
// already refuses assigning a role to yourself; these are the three other
// routes to the same elevation: adding actions to your own role's bundle,
// reactivating your own role, and reactivating one of its bundles.

const ownRoleTenant = "11111111-1111-4111-8111-111111111111"

func ownRoleStore(held ...domain.PrincipalRoleAssignment) *stubStore {
	return &stubStore{
		role:            &domain.Role{RoleID: "r-1", TenantID: ownRoleTenant, RoleScopeType: "TENANT"},
		setActiveRole:   &domain.Role{RoleID: "r-1", TenantID: ownRoleTenant},
		bundle:          &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1"},
		bundleCreated:   true,
		bundleActive:    &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1"},
		listAssignments: held,
	}
}

func held(effectiveTo *time.Time) domain.PrincipalRoleAssignment {
	return domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "admin-1", RoleID: "r-1", EffectiveTo: effectiveTo}
}

func postAsAdmin(store *stubStore, path, body string) *httptest.ResponseRecorder {
	r := newTestRouter(store)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", ownRoleTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const bundleBody = `{"bundle_code":"ESCALATE","permitted_actions":["iam.assignment.grant","PAYMENT_RELEASE"]}`

func TestOwnRole_BundleOnHeldRoleRefused(t *testing.T) {
	store := ownRoleStore(held(nil))
	w := postAsAdmin(store, "/v1/admin/roles/r-1/permission-bundles", bundleBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("bundle on a role the caller holds: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotAssignPrincipal != "admin-1" || store.gotAssignRole != "r-1" || store.gotAssignTenant != ownRoleTenant {
		t.Errorf("own-role lookup asked about principal=%q role=%q tenant=%q", store.gotAssignPrincipal, store.gotAssignRole, store.gotAssignTenant)
	}
}

// A future-dated assignment is held too: widening the role now elevates the
// caller the moment it starts.
func TestOwnRole_FutureDatedAssignmentCounts(t *testing.T) {
	store := ownRoleStore(held(nil))
	store.listAssignments[0].EffectiveFrom = time.Now().Add(24 * time.Hour)
	if w := postAsAdmin(store, "/v1/admin/roles/r-1/permission-bundles", bundleBody); w.Code != http.StatusForbidden {
		t.Fatalf("future-dated holder: want 403, got %d", w.Code)
	}
	if store.gotAssignActiveOnly {
		t.Error("own-role lookup filtered to active-only, which hides future-dated assignments")
	}
}

func TestOwnRole_ScheduledEndStillHeld(t *testing.T) {
	end := time.Now().Add(time.Hour)
	if w := postAsAdmin(ownRoleStore(held(&end)), "/v1/admin/roles/r-1/permission-bundles", bundleBody); w.Code != http.StatusForbidden {
		t.Fatalf("assignment ending in an hour: want 403, got %d", w.Code)
	}
}

func TestOwnRole_EndedAssignmentDoesNotCount(t *testing.T) {
	ended := time.Now().Add(-time.Hour)
	if w := postAsAdmin(ownRoleStore(held(&ended)), "/v1/admin/roles/r-1/permission-bundles", bundleBody); w.Code != http.StatusCreated {
		t.Fatalf("ended assignment: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnRole_NonHolderMayAddBundle(t *testing.T) {
	if w := postAsAdmin(ownRoleStore(), "/v1/admin/roles/r-1/permission-bundles", bundleBody); w.Code != http.StatusCreated {
		t.Fatalf("non-holder: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnRole_ReactivateHeldRoleRefused(t *testing.T) {
	store := ownRoleStore(held(nil))
	if w := postAsAdmin(store, "/v1/admin/roles/r-1/reactivate", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("reactivate own role: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if store.setActiveCalls != 0 {
		t.Error("SetRoleActive was reached despite the refusal")
	}
}

// Retiring narrows access, so a holder may retire their own role.
func TestOwnRole_RetireHeldRoleAllowed(t *testing.T) {
	if w := postAsAdmin(ownRoleStore(held(nil)), "/v1/admin/roles/r-1/retire", `{}`); w.Code != http.StatusOK {
		t.Fatalf("retire own role: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnRole_ReactivateBundleOfHeldRoleRefused(t *testing.T) {
	store := ownRoleStore(held(nil))
	if w := postAsAdmin(store, "/v1/admin/permission-bundles/b-1/reactivate", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("reactivate own role's bundle: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if store.bundleActiveCalls != 0 {
		t.Error("SetPermissionBundleActive was reached despite the refusal")
	}
	if store.gotAssignRole != "r-1" {
		t.Errorf("own-role lookup used role %q, want the bundle's role r-1", store.gotAssignRole)
	}
}

func TestOwnRole_RetireBundleOfHeldRoleAllowed(t *testing.T) {
	if w := postAsAdmin(ownRoleStore(held(nil)), "/v1/admin/permission-bundles/b-1/retire", `{}`); w.Code != http.StatusOK {
		t.Fatalf("retire own role's bundle: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnRole_ReactivateUnknownBundle404(t *testing.T) {
	store := ownRoleStore()
	store.findBundleErr = domain.ErrPermissionBundleNotFound
	if w := postAsAdmin(store, "/v1/admin/permission-bundles/b-x/reactivate", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown bundle: want 404, got %d", w.Code)
	}
}

// Fails closed: if holding cannot be established, the write is refused.
func TestOwnRole_LookupFailureRefuses(t *testing.T) {
	store := ownRoleStore()
	store.listAssignmentsErr = errors.New("db down")
	if w := postAsAdmin(store, "/v1/admin/roles/r-1/permission-bundles", bundleBody); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure: want 503, got %d", w.Code)
	}
}
