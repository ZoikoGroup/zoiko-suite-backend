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
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS access_decision_log, sod_rules, delegated_authorities, principal_role_assignments, permission_bundles, roles CASCADE;")

	mig1, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 1: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig1)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
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
		PrincipalID: principalID, RoleID: role.RoleID, LegalEntityID: legalEntityID,
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

	actions, basis, err := s.FindGrantedActions(ctx, "principal-1", "00000000-0000-0000-0000-0000000000e1")
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
	actions, _, err = s.FindGrantedActions(ctx, "principal-1", "00000000-0000-0000-0000-0000000000e2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no grant in a different entity, got %v", actions)
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
		PrincipalID: "principal-1", RoleID: role.RoleID, LegalEntityID: legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour), AssignedBy: "admin-1",
	})
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	actions, _, _ := s.FindGrantedActions(ctx, "principal-1", legalEntityID)
	if len(actions) != 1 {
		t.Fatalf("expected grant before revoke, got %v", actions)
	}

	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	actions, _, _ = s.FindGrantedActions(ctx, "principal-1", legalEntityID)
	if len(actions) != 0 {
		t.Fatalf("expected no grant after revoke, got %v", actions)
	}

	// Revoking again should 404, not silently succeed twice.
	if _, err := s.RevokeRoleAssignment(ctx, assignment.PrincipalRoleAssignmentID); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
		t.Fatalf("expected ErrRoleAssignmentNotFound on double-revoke, got %v", err)
	}
}

func TestPgStore_DelegatedAuthority_RevocationIsOneWay(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	d, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "principal-1", ScopeType: "FULL",
		LegalEntityID: legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	if d.RevocationStatus != "ACTIVE" {
		t.Fatalf("expected ACTIVE, got %s", d.RevocationStatus)
	}

	revoked, err := s.RevokeDelegatedAuthority(ctx, d.DelegatedAuthorityID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.RevocationStatus != "REVOKED" {
		t.Fatalf("expected REVOKED, got %s", revoked.RevocationStatus)
	}

	// Revoking an already-revoked delegation must fail, not silently succeed.
	if _, err := s.RevokeDelegatedAuthority(ctx, d.DelegatedAuthorityID); !errors.Is(err, domain.ErrInvalidTransition) {
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
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	actions, basis, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID)
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

	conflicting, hasConflict, err := s.CheckSoDConflict(ctx, []string{"PAYMENT_INITIATE"}, "PAYMENT_APPROVE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasConflict || conflicting != "PAYMENT_INITIATE" {
		t.Fatalf("expected conflict with PAYMENT_INITIATE, got conflict=%v action=%s", hasConflict, conflicting)
	}

	_, hasConflict, err = s.CheckSoDConflict(ctx, []string{"PAYMENT_VIEW"}, "PAYMENT_APPROVE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasConflict {
		t.Fatalf("expected no conflict for unrelated actions")
	}
}

func TestPgStore_AccessDecisionLog_RecordAndRetrieve(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	d, err := s.RecordAccessDecision(ctx, "principal-1", "00000000-0000-0000-0000-0000000000e1", "PAYMENT_APPROVE", "DENIED", "no_grant", "corr-1")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if d.DecisionOutcome != "DENIED" {
		t.Fatalf("expected DENIED, got %s", d.DecisionOutcome)
	}

	found, err := s.FindAccessDecisionByID(ctx, d.AccessDecisionID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.DecisionBasis != "no_grant" {
		t.Fatalf("expected basis no_grant, got %s", found.DecisionBasis)
	}
}

func TestPgStore_FindAccessDecisionByID_NotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.FindAccessDecisionByID(context.Background(), "00000000-0000-0000-0000-000000000099")
	if !errors.Is(err, domain.ErrAccessDecisionNotFound) {
		t.Fatalf("expected ErrAccessDecisionNotFound, got %v", err)
	}
}
