package store_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"zoiko.io/authorization-svc/internal/domain"
	store "zoiko.io/authorization-svc/internal/store"
)

func TestPgStore_HierarchicalScope_ScenarioA03_BookSpecificDenial(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000001"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	bookA := "00000000-0000-0000-0000-0000000000b1"
	bookB := "00000000-0000-0000-0000-0000000000b2"
	principalID := "accountant-1"

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID:             tenantID,
		RoleCode:             "BOOK_POSTER",
		RoleName:             "Book Poster",
		RoleScopeType:        "LEGAL_ENTITY",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create role: %v", err)
	}

	_, _, err = s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID:           role.RoleID,
		BundleCode:       "POSTING_BUNDLE",
		PermittedActions: []string{"gl.journal.post", "gl.journal.view"},
	})
	if err != nil {
		t.Fatalf("failed to create bundle: %v", err)
	}

	// Create assignment strictly scoped to Book A
	assignment, err := s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID:   principalID,
		RoleID:        role.RoleID,
		LegalEntityID: &legalEntityID,
		BookID:        &bookA,
		EffectiveFrom: time.Now().Add(-time.Hour),
		AssignedBy:    "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create assignment: %v", err)
	}
	if assignment.BookID == nil || *assignment.BookID != bookA {
		t.Fatalf("expected assignment to have BookID %s, got %v", bookA, assignment.BookID)
	}

	// 1. Check access in Book A: should be GRANTED
	actions, basis, err := s.FindGrantedActionsScoped(ctx, principalID, legalEntityID, tenantID, bookA, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 || basis == "" {
		t.Fatalf("expected actions granted in Book A, got actions=%v, basis=%q", actions, basis)
	}

	// 2. Scenario A03: Attempt access in Book B (same entity, wrong book) -> must be DENIED (no actions returned)
	actionsB, _, err := s.FindGrantedActionsScoped(ctx, principalID, legalEntityID, tenantID, bookB, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actionsB) != 0 {
		t.Fatalf("Scenario A03 violation: expected DENIAL (0 actions) for Book B, got %v", actionsB)
	}

	// 3. Unscoped book request when role is book-scoped -> should NOT match book-scoped role
	actionsEmpty, _, err := s.FindGrantedActionsScoped(ctx, principalID, legalEntityID, tenantID, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actionsEmpty) != 0 {
		t.Fatalf("expected 0 actions for unscoped request against book-scoped grant, got %v", actionsEmpty)
	}
}

func TestPgStore_HierarchicalScope_EntityWideMatchesAnyBook(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000001"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	bookA := "00000000-0000-0000-0000-0000000000b1"
	bookB := "00000000-0000-0000-0000-0000000000b2"
	principalID := "controller-1"

	role, _, err := s.CreateRole(ctx, domain.CreateRoleParams{
		TenantID:             tenantID,
		RoleCode:             "FINANCIAL_CONTROLLER",
		RoleName:             "Financial Controller",
		RoleScopeType:        "LEGAL_ENTITY",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create role: %v", err)
	}

	_, _, err = s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{
		RoleID:           role.RoleID,
		BundleCode:       "CONTROLLER_BUNDLE",
		PermittedActions: []string{"gl.close.run"},
	})
	if err != nil {
		t.Fatalf("failed to create bundle: %v", err)
	}

	// Entity-wide assignment (BookID is nil)
	_, err = s.CreateRoleAssignment(ctx, domain.CreateRoleAssignmentParams{
		PrincipalID:   principalID,
		RoleID:        role.RoleID,
		LegalEntityID: &legalEntityID,
		BookID:        nil,
		EffectiveFrom: time.Now().Add(-time.Hour),
		AssignedBy:    "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create assignment: %v", err)
	}

	// Entity-wide grant matches Book A
	actionsA, _, err := s.FindGrantedActionsScoped(ctx, principalID, legalEntityID, tenantID, bookA, "")
	if err != nil || len(actionsA) == 0 {
		t.Fatalf("expected entity-wide grant to match Book A, got %v (err: %v)", actionsA, err)
	}

	// Entity-wide grant matches Book B
	actionsB, _, err := s.FindGrantedActionsScoped(ctx, principalID, legalEntityID, tenantID, bookB, "")
	if err != nil || len(actionsB) == 0 {
		t.Fatalf("expected entity-wide grant to match Book B, got %v (err: %v)", actionsB, err)
	}
}

func TestPgStore_AuthorityLimits_CRUD(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000001"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	bookID := "00000000-0000-0000-0000-0000000000b1"
	principalID := "cfo-1"

	limit, err := s.CreateAuthorityLimit(ctx, domain.CreateAuthorityLimitParams{
		TenantID:      tenantID,
		PrincipalID:   &principalID,
		AuthorityType: "payment_release",
		LegalEntityID: &legalEntityID,
		BookID:        &bookID,
		Currency:      "GBP",
		LowerLimit:    "0",
		UpperLimit:    "100000.00",
		EffectiveFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("failed to create authority limit: %v", err)
	}
	if limit.AuthorityLimitID == "" {
		t.Fatalf("expected non-empty authority limit id")
	}
	if limit.UpperLimit != "100000.0000" && limit.UpperLimit != "100000.00" {
		t.Errorf("unexpected upper limit: %s", limit.UpperLimit)
	}

	// Find by ID
	found, err := s.FindAuthorityLimitByID(ctx, limit.AuthorityLimitID, tenantID)
	if err != nil {
		t.Fatalf("failed to find authority limit by id: %v", err)
	}
	if found.AuthorityType != "payment_release" {
		t.Errorf("expected authority_type payment_release, got %s", found.AuthorityType)
	}

	// List limits
	limits, err := s.ListAuthorityLimits(ctx, tenantID, principalID, "", "payment_release")
	if err != nil {
		t.Fatalf("failed to list authority limits: %v", err)
	}
	if len(limits) != 1 {
		t.Fatalf("expected 1 limit in list, got %d", len(limits))
	}
}
