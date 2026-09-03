package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	// access_decision_log is PARTITIONED since 000009, so its partitions must
	// go too — DROP TABLE on the parent takes them, but a partition left over
	// from a previous run under a different name would collide with the
	// migration's CREATE. The two helper functions and the view are dropped
	// for the same reason: 000009 creates them, and CREATE OR REPLACE FUNCTION
	// cannot change a function's return type, so a stale definition from an
	// earlier schema version fails the migration rather than being replaced.
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS abac_rules, access_decision_log, sod_rules, delegated_authorities, principal_role_assignments, permission_bundles, roles CASCADE;")
	_, _ = pool.Exec(ctx, "DROP VIEW IF EXISTS access_decision_log_retention_status;")
	_, _ = pool.Exec(ctx, "DROP FUNCTION IF EXISTS detach_access_decision_log_partitions_before(DATE);")
	_, _ = pool.Exec(ctx, "DROP FUNCTION IF EXISTS create_access_decision_log_partition(DATE);")
	// Detached partitions are no longer children of the parent, so DROP TABLE
	// on it does not reach them.
	_, _ = pool.Exec(ctx, `DO $$
		DECLARE t text;
		BEGIN
			FOR t IN SELECT tablename FROM pg_tables
			          WHERE schemaname = current_schema()
			            AND tablename LIKE 'access_decision_log%'
			LOOP
				EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t);
			END LOOP;
		END $$;`)

	// Every up migration, in order. This was five copy-pasted blocks naming
	// four files, and 000005 was never added when it landed — so
	// access_decision_log had no tenant_id here and both AccessDecisionLog
	// tests failed on a column the real schema has. A list is the shape that
	// makes the next migration a one-line change instead of a missed one.
	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_sod_rule_tenant_scoping.up.sql",
		"000003_nullable_legal_entity_for_tenant_scope.up.sql",
		"000004_add_rls.up.sql",
		"000005_add_access_decision_tenant.up.sql",
		"000006_add_delegation_tenant.up.sql",
		// 000007 was landed but never added here, so every test in this file
		// ran against a schema where permission_bundles and
		// principal_role_assignments had no row security at all — which is
		// most of what the RLS tests below are about.
		"000007_add_rls_bundles_assignments.up.sql",
		"000008_fix_delegation_evaluation.up.sql",
		"000009_partition_access_decision_log.up.sql",
		"000010_add_abac_rules.up.sql",
	} {
		sql, err := os.ReadFile("../../deployments/migrations/" + name)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", name, err)
		}
	}
}

// setupRoleWithGrant creates a role + bundle + active assignment for
// principalID in legalEntityID granting actions, returning the role_id.
func setupRoleWithGrant(t *testing.T, s *store.PgStore, tenantID, principalID, legalEntityID, roleCode string, actions []string) string {
	t.Helper()
	ctx := context.Background()

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: tenantID, RoleCode: roleCode, RoleName: roleCode, RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: role.RoleID, BundleCode: "default", PermittedActions: actions,
	}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if _, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: principalID, RoleID: role.RoleID, LegalEntityID: &legalEntityID,
		EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	return role.RoleID
}

func TestPgStore_CreateRole_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	params := domain.CreateRoleParams{TenantID: "00000000-0000-0000-0000-000000000001", RoleCode: "FINANCE_APPROVER", RoleName: "Finance Approver", RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1"}

	r1, created, err := s.CreateRole(ctx, params)
	if err != nil || !created {
		t.Fatalf("expected created=true, err=nil; got created=%v err=%v", created, err)
	}

	r2, created, err := s.CreateRole(ctx, params)
	if err != nil || created {
		t.Fatalf("expected idempotent no-op; got created=%v err=%v", created, err)
	}
	if r2.RoleID != r1.RoleID {
		t.Errorf("expected same role ID on replay")
	}

	conflicting := params
	conflicting.RoleName = "Different Name"
	_, _, err = s.CreateRole(ctx, conflicting)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestPgStore_FindGrantedActions_RBAC(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	setupRoleWithGrant(t, s, "00000000-0000-0000-0000-000000000001", "principal-1", "00000000-0000-0000-0000-0000000000e1", "FINANCE_APPROVER", []string{"PAYMENT_APPROVE", "PAYMENT_VIEW"})

	actions, basis, err := s.FindGrantedActions(ctx, "principal-1", "00000000-0000-0000-0000-0000000000e1", "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 granted actions, got %v", actions)
	}
	if basis == "" {
		t.Errorf("expected non-empty basis")
	}

	// Different entity — no grant.
	actions, _, err = s.FindGrantedActions(ctx, "principal-1", "00000000-0000-0000-0000-0000000000e2", "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no grant in a different entity, got %v", actions)
	}
}

func TestPgStore_CreateRoleAssignment_TenantWideRequiresTenantScopedRole(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"

	entityRole, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: tenantID, RoleCode: "ENTITY_ROLE", RoleName: "Entity Role", RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: entityRole.RoleID, LegalEntityID: nil,
		EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	}); !errors.Is(err, domain.ErrLegalEntityRequiredForRoleScope) {
		t.Fatalf("expected ErrLegalEntityRequiredForRoleScope for a LEGAL_ENTITY-scoped role with no legal_entity_id, got %v", err)
	}

	tenantRole, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: tenantID, RoleCode: "TENANT_ROLE", RoleName: "Tenant Role", RoleScopeType: "TENANT", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: tenantRole.RoleID, BundleCode: "default", PermittedActions: []string{"PLATFORM_ADMIN"},
	}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if _, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: tenantRole.RoleID, LegalEntityID: nil,
		EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	}); err != nil {
		t.Fatalf("expected tenant-wide assignment to succeed for a TENANT-scoped role, got: %v", err)
	}

	// A tenant-wide grant must be visible when evaluating ANY legal entity.
	for _, entity := range []string{"00000000-0000-0000-0000-0000000000e1", "00000000-0000-0000-0000-0000000000e2"} {
		actions, _, err := s.FindGrantedActions(ctx, "principal-1", entity, tenantID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(actions) != 1 || actions[0] != "PLATFORM_ADMIN" {
			t.Fatalf("expected tenant-wide grant to apply in entity %s, got %v", entity, actions)
		}
	}
}

func TestPgStore_RevokeRoleAssignment_EndsGrant(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000001"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	role, _, _ := s.CreateRole(ctx, domain.CreateRoleParams{TenantID: tenantID, RoleCode: "R1", RoleName: "R1", RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1"})
	_, _ = s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_X"}})
	assignment, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: role.RoleID, LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	})
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	actions, _, _ := s.FindGrantedActions(ctx, "principal-1", legalEntityID, tenantID)
	if len(actions) != 1 {
		t.Fatalf("expected grant before revoke, got %v", actions)
	}

	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID, tenantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	actions, _, _ = s.FindGrantedActions(ctx, "principal-1", legalEntityID, tenantID)
	if len(actions) != 0 {
		t.Fatalf("expected no grant after revoke, got %v", actions)
	}

	// Revoking again should 404, not silently succeed twice.
	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID, tenantID); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
		t.Fatalf("expected ErrRoleAssignmentNotFound on double-revoke, got %v", err)
	}
}

// TestPgStore_TenantIsolation_RevokeRoleAssignment proves the real fix
// in this row: tenant B must not be able to revoke tenant A's role
// assignment by ID alone, at the DB layer — RevokeRoleAssignment's own
// tenant-via-role-join predicate, not just the handler's app-level check.
func TestPgStore_TenantIsolation_RevokeRoleAssignment(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-0000000000a1"
	tenantB := "00000000-0000-0000-0000-0000000000b1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{TenantID: tenantA, RoleCode: "R1", RoleName: "R1", RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_X"}}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	assignment, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: role.RoleID, LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	})
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Probe: tenant B's context, tenant A's assignment ID.
	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID, tenantB); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
		t.Fatalf("ISOLATION FAILURE: expected ErrRoleAssignmentNotFound revoking tenant A's assignment under tenant B, got %v", err)
	}

	// Verify tenant A's grant is genuinely still active.
	actions, _, err := s.FindGrantedActions(ctx, "principal-1", legalEntityID, tenantA)
	if err != nil || len(actions) == 0 {
		t.Fatalf("ISOLATION FAILURE: tenant A's grant was revoked by tenant B's attempt, actions=%v err=%v", actions, err)
	}

	// Sanity: tenant A can still revoke its own assignment.
	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID, tenantA); err != nil {
		t.Fatalf("expected tenant A to revoke its own assignment, got %v", err)
	}
}

// TestPgStore_PlatformScope_FindGrantedActionsAcrossTenants proves the
// platform-scope bypass on FindGrantedActions actually works: this is
// the core /v1/authorize evaluation path, called without any reliable
// tenant context, and it must still resolve a real grant for a role that
// belongs to a real, specific tenant. Without this test, a broken bypass
// would silently deny every authorization check platform-wide the moment
// zoiko_app (a genuine non-owner role) enforces FORCE ROW LEVEL SECURITY
// for real.
func TestPgStore_PlatformScope_FindGrantedActionsAcrossTenants(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{TenantID: tenantA, RoleCode: "R1", RoleName: "R1", RoleScopeType: "LEGAL_ENTITY", CreatedByPrincipalID: "admin-1"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "default", PermittedActions: []string{"ACTION_X"}}); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if _, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: role.RoleID, LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// No tenant is passed here at all — exactly how /v1/authorize's
	// handler actually calls this today.
	actions, basis, err := s.FindGrantedActions(context.Background(), "principal-1", legalEntityID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "ACTION_X" {
		t.Fatalf("expected the tenant A role's grant to resolve with no tenant context supplied, got %v (basis=%s)", actions, basis)
	}
}

func TestPgStore_DelegatedAuthority_RevocationIsOneWay(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	tenantID := "00000000-0000-0000-0000-000000000001"
	d, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "principal-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	if d.RevocationStatus != "ACTIVE" {
		t.Fatalf("expected ACTIVE, got %s", d.RevocationStatus)
	}

	revoked, err := s.RevokeDelegatedAuthority(ctx, d.DelegatedAuthorityID, tenantID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.RevocationStatus != "REVOKED" {
		t.Fatalf("expected REVOKED, got %s", revoked.RevocationStatus)
	}

	// Revoking an already-revoked delegation must fail, not silently succeed.
	if _, err := s.RevokeDelegatedAuthority(ctx, d.DelegatedAuthorityID, tenantID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition on double-revoke, got %v", err)
	}
}

func TestPgStore_FindDelegatedActions_ResolvesViaDelegator(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, s, "00000000-0000-0000-0000-000000000001", "boss-1", legalEntityID, "FINANCE_APPROVER", []string{"PAYMENT_APPROVE"})

	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             "00000000-0000-0000-0000-000000000001",
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	actions, basis, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("expected delegated PAYMENT_APPROVE, got %v", actions)
	}
	if basis == "" {
		t.Errorf("expected non-empty delegation basis")
	}
}

func TestPgStore_CheckSoDConflict(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	if _, err := s.CreateSoDRule(ctx, domain.CreateSoDRuleParams{
		DomainCode: "FINANCE", ActionA: "PAYMENT_INITIATE", ActionB: "PAYMENT_APPROVE", ConflictType: "MUTUALLY_EXCLUSIVE",
	}); err != nil {
		t.Fatalf("create sod rule: %v", err)
	}

	conflicting, hasConflict, err := s.CheckSoDConflict(ctx, []string{"PAYMENT_INITIATE"}, "PAYMENT_APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasConflict || conflicting != "PAYMENT_INITIATE" {
		t.Fatalf("expected conflict with PAYMENT_INITIATE, got conflict=%v action=%s", hasConflict, conflicting)
	}

	_, hasConflict, err = s.CheckSoDConflict(ctx, []string{"PAYMENT_VIEW"}, "PAYMENT_APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasConflict {
		t.Fatalf("expected no conflict for unrelated actions")
	}
}

func TestPgStore_CheckSoDConflict_TenantScoping(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	if _, err := s.CreateSoDRule(ctx, domain.CreateSoDRuleParams{
		DomainCode: "HR", ActionA: "OFFBOARD_INITIATE", ActionB: "OFFBOARD_APPROVE", ConflictType: "MUTUALLY_EXCLUSIVE",
		TenantID: &tenantA,
	}); err != nil {
		t.Fatalf("create tenant-scoped sod rule: %v", err)
	}

	// Same principal, same held action, different tenants: tenant A's rule
	// must apply for tenant A and must NOT leak into tenant B's evaluation.
	_, hasConflict, err := s.CheckSoDConflict(ctx, []string{"OFFBOARD_INITIATE"}, "OFFBOARD_APPROVE", tenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasConflict {
		t.Fatalf("expected tenant A's SoD rule to apply within tenant A")
	}

	_, hasConflict, err = s.CheckSoDConflict(ctx, []string{"OFFBOARD_INITIATE"}, "OFFBOARD_APPROVE", tenantB)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasConflict {
		t.Fatalf("tenant A's SoD rule must not apply within tenant B")
	}

	// Omitting tenant_id entirely (empty string, the pre-existing calling
	// convention) must not accidentally match a tenant-scoped rule either.
	_, hasConflict, err = s.CheckSoDConflict(ctx, []string{"OFFBOARD_INITIATE"}, "OFFBOARD_APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasConflict {
		t.Fatalf("a tenant-scoped rule must not apply when no tenant is supplied")
	}
}

// TestPgStore_CheckOwnObjectSoD proves the domain.ConflictTypeOwnObjectForbidden
// convention: a self-referential sod_rules row (action_a == action_b)
// forbids the action via CheckOwnObjectSoD. It also proves CheckSoDConflict
// is blind to that same row when the caller has already excluded the
// candidate action from its held-actions set — exactly what the real
// /v1/authorize handler does via removeAll before calling it — showing the
// two queries answer genuinely distinct questions rather than one
// silently duplicating or masking the other.
func TestPgStore_CheckOwnObjectSoD(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	if _, err := s.CreateSoDRule(ctx, domain.CreateSoDRuleParams{
		DomainCode: "AP", ActionA: "AP_INVOICE_APPROVE", ActionB: "AP_INVOICE_APPROVE",
		ConflictType: domain.ConflictTypeOwnObjectForbidden,
	}); err != nil {
		t.Fatalf("create own-object sod rule: %v", err)
	}

	forbidden, err := s.CheckOwnObjectSoD(ctx, "AP_INVOICE_APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !forbidden {
		t.Fatalf("expected AP_INVOICE_APPROVE to be own-object forbidden")
	}

	forbidden, err = s.CheckOwnObjectSoD(ctx, "AP_INVOICE_ISSUE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forbidden {
		t.Fatalf("expected AP_INVOICE_ISSUE to have no own-object rule")
	}

	// The real /v1/authorize handler always removes the candidate action
	// from the caller's held-actions set before calling CheckSoDConflict
	// (see Authorize's removeAll call) — that removal, not this query, is
	// what keeps a self-referential own-object rule from being visible to
	// the static-conflict path. So the correct simulation of that caller
	// behavior is an EMPTY held-actions set (the candidate was the only
	// thing held), not a set that still contains the candidate action.
	conflicting, hasConflict, err := s.CheckSoDConflict(ctx, []string{}, "AP_INVOICE_APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasConflict {
		t.Fatalf("CheckSoDConflict must not report a conflict when the candidate action has already been excluded from held actions (got conflict with %s)", conflicting)
	}
}

func TestPgStore_AccessDecisionLog_RecordAndRetrieve(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	// tenant_id is a UUID column since 000005, and a decision recorded with
	// NO tenant is deliberately unreadable — so this records one WITH a tenant
	// and reads it back under the same scope. The original form of this test
	// recorded no tenant and expected to find it, which is the behaviour
	// 000005 removed on purpose.
	tenantID := "00000000-0000-0000-0000-000000000001"
	otherTenant := "00000000-0000-0000-0000-000000000002"

	d, err := s.RecordAccessDecision(ctx, domain.RecordAccessDecisionParams{
		PrincipalID:   "principal-1",
		LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		ActionType:    "PAYMENT_APPROVE",
		Outcome:       "DENIED",
		Basis:         "no_grant",
		CorrelationID: "corr-1",
		TenantID:      tenantID,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if d.DecisionOutcome != "DENIED" {
		t.Fatalf("expected DENIED, got %s", d.DecisionOutcome)
	}

	found, err := s.FindAccessDecisionByID(ctx, d.AccessDecisionID, tenantID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.DecisionBasis != "no_grant" {
		t.Fatalf("expected basis no_grant, got %s", found.DecisionBasis)
	}

	// Another tenant must not be able to read it, and must not be able to tell
	// it exists — 404, not 403.
	if _, err := s.FindAccessDecisionByID(ctx, d.AccessDecisionID, otherTenant); !errors.Is(err, domain.ErrAccessDecisionNotFound) {
		t.Fatalf("ISOLATION FAILURE: expected ErrAccessDecisionNotFound reading tenant 1's decision as tenant 2, got %v", err)
	}

	// A decision recorded with no tenant at all is readable by nobody.
	anon, err := s.RecordAccessDecision(ctx, domain.RecordAccessDecisionParams{
		PrincipalID:   "principal-2",
		LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		ActionType:    "PAYMENT_APPROVE",
		Outcome:       "DENIED",
		Basis:         "no_grant",
		CorrelationID: "corr-2",
	})
	if err != nil {
		t.Fatalf("record untenanted: %v", err)
	}
	if _, err := s.FindAccessDecisionByID(ctx, anon.AccessDecisionID, tenantID); !errors.Is(err, domain.ErrAccessDecisionNotFound) {
		t.Fatalf("expected a NULL-tenant decision to be unreadable, got %v", err)
	}
}

func TestPgStore_FindAccessDecisionByID_NotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.FindAccessDecisionByID(context.Background(), "00000000-0000-0000-0000-000000000099", "00000000-0000-0000-0000-000000000001")
	if !errors.Is(err, domain.ErrAccessDecisionNotFound) {
		t.Fatalf("expected ErrAccessDecisionNotFound, got %v", err)
	}
}

// TestPgStore_FindGrantedActions_DoesNotLeakAcrossTenants is the regression
// guard for the cross-tenant privilege escalation that platform-scoping this
// query unconditionally used to allow.
//
// Setup is the shape that made it exploitable: a TENANT-WIDE assignment
// (legal_entity_id IS NULL) in tenant A, which the assignment predicate's
// `OR pra.legal_entity_id IS NULL` matches for ANY legal entity — including
// one belonging to tenant B. Before the fix, evaluating principal-1 against
// tenant B returned tenant A's PAYROLL_RUN_FINALIZE.
func TestPgStore_FindGrantedActions_DoesNotLeakAcrossTenants(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-0000000000a1"
	tenantB := "00000000-0000-0000-0000-0000000000b1"
	entityInB := "00000000-0000-0000-0000-0000000000eb"

	// Tenant A: a tenant-wide role carrying a materially dangerous action.
	roleA, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID: tenantA, RoleCode: "A_ADMIN", RoleName: "A Admin", RoleScopeType: "TENANT", CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create tenant A role: %v", err)
	}
	if _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID: roleA.RoleID, BundleCode: "default", PermittedActions: []string{"PAYROLL_RUN_FINALIZE"},
	}); err != nil {
		t.Fatalf("create tenant A bundle: %v", err)
	}
	if _, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID: "principal-1", RoleID: roleA.RoleID, LegalEntityID: nil,
		EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	}); err != nil {
		t.Fatalf("create tenant A tenant-wide assignment: %v", err)
	}

	// Tenant B: the same principal, holding only a harmless action.
	setupRoleWithGrant(t, s, tenantB, "principal-1", entityInB, "B_VIEWER", []string{"REPORT_VIEW"})

	actions, _, err := s.FindGrantedActions(ctx, "principal-1", entityInB, tenantB)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range actions {
		if a == "PAYROLL_RUN_FINALIZE" {
			t.Fatalf("ISOLATION FAILURE: tenant A's tenant-wide grant resolved while evaluating tenant B, got %v", actions)
		}
	}
	if len(actions) != 1 || actions[0] != "REPORT_VIEW" {
		t.Fatalf("expected only tenant B's own grant, got %v", actions)
	}
}

// TestPgStore_TenantIsolation_DelegatedAuthority is the regression guard for
// the gap 000006 closed. Before it, delegated_authorities had no tenant_id and
// therefore no policy, and every read, revoke and evaluation of a delegation
// ran with no tenant predicate at all.
//
// Each leg below failed before the column existed.
func TestPgStore_TenantIsolation_DelegatedAuthority(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-0000000000a1"
	tenantB := "00000000-0000-0000-0000-0000000000b1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"

	// Tenant A's boss delegates to their assistant.
	setupRoleWithGrant(t, s, tenantA, "boss-1", legalEntityID, "FINANCE_APPROVER", []string{"PAYMENT_APPROVE"})
	d, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantA,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	if d.TenantID != tenantA {
		t.Fatalf("expected the delegation to carry tenant A, got %q", d.TenantID)
	}

	// 1. Tenant B must not be able to read it — and must get "not found", not
	//    "forbidden", so a probe cannot confirm the id exists.
	if _, err := s.FindDelegatedAuthorityByID(ctx, d.DelegatedAuthorityID, tenantB); !errors.Is(err, domain.ErrDelegatedAuthorityNotFound) {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's delegation, got %v", err)
	}

	// 2. Nor revoke it. This is the one that mattered most: revoking someone
	//    else's delegation silently removes an authority they are relying on.
	if _, err := s.RevokeDelegatedAuthority(ctx, d.DelegatedAuthorityID, tenantB); !errors.Is(err, domain.ErrDelegatedAuthorityNotFound) {
		t.Fatalf("ISOLATION FAILURE: tenant B revoked tenant A's delegation, got %v", err)
	}

	// 3. Nor have it resolve during authorization under their own scope.
	actions, _, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantB)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("ISOLATION FAILURE: tenant A's delegation granted %v while evaluating tenant B", actions)
	}

	// 4. Tenant A's own use of it is unaffected.
	actions, _, err = s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("expected tenant A's own delegation to still resolve, got %v", actions)
	}

	// 5. A delegation with no tenant is refused rather than written as a row
	//    no policy could ever match.
	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-2", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	}); !errors.Is(err, domain.ErrTenantScopeRequired) {
		t.Fatalf("expected ErrTenantScopeRequired for a tenantless delegation, got %v", err)
	}
}
