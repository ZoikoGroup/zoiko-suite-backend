package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

// Integration tests for the permission-bundle read and off switch, against
// real PostgreSQL.
//
// The load-bearing one is TestPgStore_SetPermissionBundleActive_WithdrawsTheGrant:
// it asserts at the EVALUATION layer, not at the store's own return value.
// `pb.active_flag` was already in FindGrantedActions' JOIN with no writer
// anywhere in the service, so a test that only checked the flag came back
// false would pass while proving nothing about whether the action stopped
// being granted — which is the entire point of the switch.

const bundleTestTenant = "00000000-0000-0000-0000-0000000000b1"
const bundleTestOtherTenant = "00000000-0000-0000-0000-0000000000b2"
const bundleTestEntity = "00000000-0000-0000-0000-0000000000e1"

func TestPgStore_CreatePermissionBundle_ReportsCreatedThenReplaced(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: bundleTestTenant, RoleCode: "BUNDLE_UPSERT", RoleName: "Bundle Upsert",
		RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	first, created, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_A", "ACTION_B"},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if !created {
		t.Error("first write reported created=false; the INSERT path must report true")
	}

	// The same bundle_code with a NARROWER action set. This is the
	// destructive edit: ACTION_B is gone from the row afterwards, and the old
	// signature gave the caller no way to tell this apart from a creation.
	second, created, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_A"},
	})
	if err != nil {
		t.Fatalf("replace bundle: %v", err)
	}
	if created {
		t.Error("the DO UPDATE path reported created=true — a replacement is not a creation")
	}
	if second.PermissionBundleID != first.PermissionBundleID {
		t.Errorf("upsert produced a new row (%s then %s); the unique index on (role_id, bundle_code) should have caught it",
			first.PermissionBundleID, second.PermissionBundleID)
	}
	if len(second.PermittedActions) != 1 || second.PermittedActions[0] != "ACTION_A" {
		t.Errorf("permitted_actions = %v; the replacement should have narrowed the set wholesale", second.PermittedActions)
	}
}

// ── ListPermissionBundles ───────────────────────────────────────────────────

func TestPgStore_ListPermissionBundles_ReturnsActionsAndRetiredLast(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: bundleTestTenant, RoleCode: "BUNDLE_LIST", RoleName: "Bundle List",
		RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	// "zz_" sorts after "aa_" by bundle_code, so if the retired one comes
	// first the ordering is by active_flag and not an accident of the codes.
	for _, b := range []struct {
		code    string
		actions []string
	}{
		{"zz_live", []string{"PAYMENT_APPROVE"}},
		{"aa_retired", []string{"PAYMENT_INITIATE"}},
	} {
		if _, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
			RoleID: role.RoleID, BundleCode: b.code, PermittedActions: b.actions,
		}); err != nil {
			t.Fatalf("create bundle %s: %v", b.code, err)
		}
	}

	all, err := s.ListPermissionBundles(ctx, role.RoleID, bundleTestTenant)
	if err != nil {
		t.Fatalf("list bundles: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 bundles, got %d", len(all))
	}

	var retiredID string
	for _, b := range all {
		if b.BundleCode == "aa_retired" {
			retiredID = b.PermissionBundleID
		}
	}
	if _, err := s.SetPermissionBundleActive(ctx, retiredID, bundleTestTenant, false); err != nil {
		t.Fatalf("retire bundle: %v", err)
	}

	after, err := s.ListPermissionBundles(ctx, role.RoleID, bundleTestTenant)
	if err != nil {
		t.Fatalf("list bundles after retire: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("a retired bundle disappeared from the list — it is why an action is gone, got %d", len(after))
	}
	if !after[0].ActiveFlag || after[1].ActiveFlag {
		t.Errorf("ordering is not active-first: got %q(active=%v) then %q(active=%v)",
			after[0].BundleCode, after[0].ActiveFlag, after[1].BundleCode, after[1].ActiveFlag)
	}
	// The actions are the reason the endpoint exists — a bundle without them
	// carries no more information than ListRoles already returned.
	if len(after[0].PermittedActions) == 0 {
		t.Error("permitted_actions came back empty")
	}
}

// The read is scoped through the role FK, so naming a role that belongs to
// another tenant returns nothing rather than that role's granted actions.
func TestPgStore_ListPermissionBundles_DoesNotCrossTenants(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: bundleTestTenant, RoleCode: "BUNDLE_SCOPED", RoleName: "Bundle Scoped",
		RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"PAYMENT_APPROVE"},
	}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	own, err := s.ListPermissionBundles(ctx, role.RoleID, bundleTestTenant)
	if err != nil {
		t.Fatalf("list in own tenant: %v", err)
	}
	if len(own) != 1 {
		t.Fatalf("expected the role's own tenant to see 1 bundle, got %d", len(own))
	}

	foreign, err := s.ListPermissionBundles(ctx, role.RoleID, bundleTestOtherTenant)
	if err != nil {
		t.Fatalf("list from another tenant: %v", err)
	}
	if len(foreign) != 0 {
		t.Errorf("another tenant read %d of this role's bundles — its granted actions leaked", len(foreign))
	}
}

// An empty tenant is refused rather than treated as "every tenant". This read
// has no legitimate platform-scope caller: it answers what a specific tenant's
// role permits, and the tenantless form would return whatever the connection
// happened to be able to see.
func TestPgStore_ListPermissionBundles_RequiresTenantScope(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())

	if _, err := s.ListPermissionBundles(context.Background(), "00000000-0000-0000-0000-0000000000aa", ""); !errors.Is(err, domain.ErrTenantScopeRequired) {
		t.Fatalf("expected ErrTenantScopeRequired for an empty tenant, got %v", err)
	}
}

// ── SetPermissionBundleActive ───────────────────────────────────────────────

// THE test for this feature. Asserted through FindGrantedActions, because the
// flag was always in that query's JOIN and what was missing was any way to set
// it — so the only assertion that means anything is that the action stops
// being granted, and comes back when the bundle is reactivated.
func TestPgStore_SetPermissionBundleActive_WithdrawsTheGrant(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const principal = "principal-bundle-1"
	roleID := setupRoleWithGrant(t, s, bundleTestTenant, principal, bundleTestEntity, "BUNDLE_WITHDRAW", []string{"ACTION_KEEP"})

	// A second bundle on the same role, so retiring one proves it withdraws
	// ONE bundle's actions and leaves the rest — which is the whole reason
	// retiring the role is not a substitute.
	if _, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: roleID, BundleCode: "extra", PermittedActions: []string{"ACTION_WITHDRAW"},
	}); err != nil {
		t.Fatalf("create second bundle: %v", err)
	}

	granted, _, err := s.FindGrantedActions(ctx, principal, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find granted: %v", err)
	}
	if !contains(granted, "ACTION_WITHDRAW") || !contains(granted, "ACTION_KEEP") {
		t.Fatalf("both actions should be granted before the retire, got %v", granted)
	}

	bundles, err := s.ListPermissionBundles(ctx, roleID, bundleTestTenant)
	if err != nil {
		t.Fatalf("list bundles: %v", err)
	}
	var extraID string
	for _, b := range bundles {
		if b.BundleCode == "extra" {
			extraID = b.PermissionBundleID
		}
	}
	if extraID == "" {
		t.Fatal("could not find the bundle to retire")
	}

	retired, err := s.SetPermissionBundleActive(ctx, extraID, bundleTestTenant, false)
	if err != nil {
		t.Fatalf("retire bundle: %v", err)
	}
	if retired.ActiveFlag {
		t.Error("active_flag still true after retire")
	}

	granted, _, err = s.FindGrantedActions(ctx, principal, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find granted after retire: %v", err)
	}
	if contains(granted, "ACTION_WITHDRAW") {
		t.Error("ACTION_WITHDRAW is still granted after its bundle was retired — the off switch does not reach the evaluation path")
	}
	if !contains(granted, "ACTION_KEEP") {
		t.Error("ACTION_KEEP was withdrawn too — retiring one bundle must not affect the others")
	}

	// And back, restoring exactly what was suspended.
	if _, err := s.SetPermissionBundleActive(ctx, extraID, bundleTestTenant, true); err != nil {
		t.Fatalf("reactivate bundle: %v", err)
	}
	granted, _, err = s.FindGrantedActions(ctx, principal, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find granted after reactivate: %v", err)
	}
	if !contains(granted, "ACTION_WITHDRAW") {
		t.Error("reactivating did not restore the action")
	}
}

// Idempotent: the caller's intent (these actions must not be granted) is
// already satisfied, and an error on a safe retry teaches an operator to
// ignore errors on a governance path.
func TestPgStore_SetPermissionBundleActive_IsIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: bundleTestTenant, RoleCode: "BUNDLE_IDEMPOTENT", RoleName: "Bundle Idempotent",
		RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	bundle, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_A"},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	for i := 1; i <= 2; i++ {
		got, err := s.SetPermissionBundleActive(ctx, bundle.PermissionBundleID, bundleTestTenant, false)
		if err != nil {
			t.Fatalf("retire #%d: %v", i, err)
		}
		if got.ActiveFlag {
			t.Errorf("retire #%d left active_flag true", i)
		}
	}
}

// 404-shaped, not a silent no-op: a bundle whose role belongs to another
// tenant reads as absent, so a probe cannot confirm the id exists and a
// cross-tenant flip cannot withdraw somebody else's grants.
func TestPgStore_SetPermissionBundleActive_DoesNotCrossTenants(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: bundleTestTenant, RoleCode: "BUNDLE_FOREIGN", RoleName: "Bundle Foreign",
		RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	bundle, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_A"},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	if _, err := s.SetPermissionBundleActive(ctx, bundle.PermissionBundleID, bundleTestOtherTenant, false); !errors.Is(err, domain.ErrPermissionBundleNotFound) {
		t.Fatalf("expected ErrPermissionBundleNotFound from another tenant, got %v", err)
	}

	// And the bundle is untouched — the refusal is not a partial write.
	still, err := s.ListPermissionBundles(ctx, role.RoleID, bundleTestTenant)
	if err != nil {
		t.Fatalf("list after refused flip: %v", err)
	}
	if len(still) != 1 || !still[0].ActiveFlag {
		t.Error("the cross-tenant flip changed the bundle anyway")
	}
}

func TestPgStore_SetPermissionBundleActive_UnknownIDIsNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())

	_, err := s.SetPermissionBundleActive(context.Background(), "00000000-0000-0000-0000-0000000000ff", bundleTestTenant, false)
	if !errors.Is(err, domain.ErrPermissionBundleNotFound) {
		t.Fatalf("expected ErrPermissionBundleNotFound for an unknown id, got %v", err)
	}
}

// Retiring a bundle must also withdraw it through a DELEGATION of the role —
// FindDelegatedActions joins permission_bundles on active_flag too, and a
// switch that stopped the direct grant while leaving the borrowed one is the
// 82a failure mode in reverse.
func TestPgStore_SetPermissionBundleActive_WithdrawsThroughDelegation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const delegator = "principal-bundle-lender"
	const delegate = "principal-bundle-borrower"
	roleID := setupRoleWithGrant(t, s, bundleTestTenant, delegator, bundleTestEntity, "BUNDLE_DELEGATED", []string{"ACTION_LENT"})

	entity := bundleTestEntity
	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID: bundleTestTenant, DelegatorPrincipalID: delegator, DelegatePrincipalID: delegate,
		LegalEntityID: &entity, ScopeType: "FULL_AUTHORITY",
		EffectiveFrom: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	borrowed, _, err := s.FindDelegatedActions(ctx, delegate, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find delegated: %v", err)
	}
	if !contains(borrowed, "ACTION_LENT") {
		t.Fatalf("the delegate should inherit ACTION_LENT before the retire, got %v", borrowed)
	}

	bundles, err := s.ListPermissionBundles(ctx, roleID, bundleTestTenant)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("list bundles: %v", err)
	}
	if _, err := s.SetPermissionBundleActive(ctx, bundles[0].PermissionBundleID, bundleTestTenant, false); err != nil {
		t.Fatalf("retire bundle: %v", err)
	}

	borrowed, _, err = s.FindDelegatedActions(ctx, delegate, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find delegated after retire: %v", err)
	}
	if contains(borrowed, "ACTION_LENT") {
		t.Error("the delegate still inherits an action whose bundle was retired — the switch does not reach the delegation layer")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// decision_basis names ROLES, and the grant query returns one row per
// (assignment x bundle) — so a role holding two bundles used to be named
// twice: `rbac:role=X,X`. Found by driving the endpoint over HTTP, because no
// test had a role with more than one bundle. That field is the audit record of
// why an action was allowed and the console renders it verbatim beside its
// paraphrase, so a duplicate is noise in the one place that has to be exact.
func TestPgStore_FindGrantedActions_BasisNamesEachRoleOnce(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const principal = "principal-basis-1"
	roleID := setupRoleWithGrant(t, s, bundleTestTenant, principal, bundleTestEntity, "BASIS_ONCE", []string{"ACTION_ONE"})

	// A second bundle on the SAME role — the condition that produced the
	// duplicate. Two bundles, two rows, one role.
	if _, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: roleID, BundleCode: "second", PermittedActions: []string{"ACTION_TWO"},
	}); err != nil {
		t.Fatalf("create second bundle: %v", err)
	}

	actions, basis, err := s.FindGrantedActions(ctx, principal, bundleTestEntity, bundleTestTenant)
	if err != nil {
		t.Fatalf("find granted: %v", err)
	}
	if !contains(actions, "ACTION_ONE") || !contains(actions, "ACTION_TWO") {
		t.Fatalf("both bundles' actions should be granted, got %v", actions)
	}

	const want = "rbac:role=BASIS_ONCE"
	if basis != want {
		t.Errorf("basis = %q, want %q — a role must be named once however many bundles it holds", basis, want)
	}
}
