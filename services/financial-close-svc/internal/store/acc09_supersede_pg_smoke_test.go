package store_test

// ACC-09's own SupersedeAllocationRule is exercised only by
// handler_test.go's in-memory stub elsewhere in this package — this file
// is its real-Postgres coverage, against the actual guarded UPDATE ...
// WHERE effective_to IS NULL clause and migration 000007's own
// idx_allocation_rules_current_version UNIQUE(rule_id) WHERE effective_to
// IS NULL constraint, not a stub's map mutation. Skips (not fails) if
// TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func approvedAllocationRule(t *testing.T, ctx context.Context, s *store.PgStore, tenantID string) *domain.AllocationRule {
	t.Helper()
	ruleID := uuid.New().String()
	rule := &domain.AllocationRule{
		RuleVersionID: ruleID, RuleID: ruleID, Version: 1, TenantID: tenantID, LegalEntityID: "le-1",
		Name: "IT shared cost allocation", SourceAccountCode: "5000-ITSharedCost",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 60},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 40},
		},
		Status: domain.AllocationRuleStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateAllocationRule(ctx, rule); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}
	if err := s.ApproveAllocationRule(ctx, rule.RuleVersionID, "approver-1", time.Now().UTC()); err != nil {
		t.Fatalf("ApproveAllocationRule: %v", err)
	}
	rule.Status = domain.AllocationRuleStatusApproved
	return rule
}

func TestPgStore_ACC09_SupersedeAllocationRule_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	original := approvedAllocationRule(t, ctx, s, tenantID)

	newVersion := &domain.AllocationRule{
		RuleVersionID: uuid.New().String(), LegalEntityID: "le-1",
		Name: "IT shared cost allocation (revised)", SourceAccountCode: "5000-ITSharedCost",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 50},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 50},
		},
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	now := time.Now().UTC()
	if err := s.SupersedeAllocationRule(ctx, original.RuleID, newVersion, now); err != nil {
		t.Fatalf("SupersedeAllocationRule: %v", err)
	}
	if newVersion.Version != 2 {
		t.Fatalf("expected version 2, got %d", newVersion.Version)
	}

	// The OLD version is now SUPERSEDED with effective_to set.
	old, err := s.GetAllocationRuleVersion(ctx, original.RuleVersionID)
	if err != nil {
		t.Fatalf("GetAllocationRuleVersion (old): %v", err)
	}
	if old.Status != domain.AllocationRuleStatusSuperseded || old.EffectiveTo == nil {
		t.Fatalf("expected the old version SUPERSEDED with effective_to set, got %+v", old)
	}

	// GetCurrentAllocationRule now resolves to the NEW version.
	current, err := s.GetCurrentAllocationRule(ctx, original.RuleID)
	if err != nil {
		t.Fatalf("GetCurrentAllocationRule: %v", err)
	}
	if current.RuleVersionID != newVersion.RuleVersionID || current.Status != domain.AllocationRuleStatusDraft {
		t.Fatalf("expected the current version to be the new DRAFT one, got %+v", current)
	}
}

// TestPgStore_ACC09_SupersedeAllocationRule_NoCurrentVersion_Refused proves
// the guarded UPDATE's WHERE clause against the real database: superseding
// a rule_id with no APPROVED/ACTIVE current version (still DRAFT, or
// already SUPERSEDED) is refused.
func TestPgStore_ACC09_SupersedeAllocationRule_NoCurrentVersion_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	ruleID := uuid.New().String()
	draft := &domain.AllocationRule{
		RuleVersionID: ruleID, RuleID: ruleID, Version: 1, TenantID: tenantID, LegalEntityID: "le-1",
		Name: "draft rule", SourceAccountCode: "5000-X",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		Status:  domain.AllocationRuleStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateAllocationRule(ctx, draft); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}

	newVersion := &domain.AllocationRule{
		RuleVersionID: uuid.New().String(), LegalEntityID: "le-1", Name: "x", SourceAccountCode: "5000-X",
		Drivers:   []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	err := s.SupersedeAllocationRule(ctx, ruleID, newVersion, time.Now().UTC())
	if err == nil {
		t.Fatal("expected SupersedeAllocationRule to refuse a rule whose current version is still DRAFT")
	}
}

// TestPgStore_ACC09_CurrentVersionUniqueConstraint_RealDB proves migration
// 000007's own idx_allocation_rules_current_version UNIQUE(rule_id) WHERE
// effective_to IS NULL is real, not just documentation — a second row for
// the same rule_id with effective_to still NULL is rejected outright.
func TestPgStore_ACC09_CurrentVersionUniqueConstraint_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	original := approvedAllocationRule(t, ctx, s, tenantID)

	// Attempt to insert a second current (effective_to IS NULL) version
	// for the SAME rule_id directly, bypassing SupersedeAllocationRule's
	// own guard entirely.
	err := pool.QueryRow(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID).Scan(new(string))
	if err != nil {
		t.Fatalf("set tenant context: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO allocation_rules (
			rule_version_id, rule_id, version, tenant_id, legal_entity_id, name,
			source_account_code, status, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uuid.New().String(), original.RuleID, 2, tenantID, "le-1", "duplicate current version",
		"5000-ITSharedCost", domain.AllocationRuleStatusDraft, time.Now().UTC(), "preparer-1")
	if err == nil {
		t.Fatal("expected the UNIQUE(rule_id) WHERE effective_to IS NULL constraint to reject a second current version")
	}
}
