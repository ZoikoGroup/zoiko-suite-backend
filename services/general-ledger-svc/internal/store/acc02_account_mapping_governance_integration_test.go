//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func newMappingDraft(tenantID, mappingKey, accountCode, creator string) *domain.AccountMapping {
	return &domain.AccountMapping{
		AccountMappingID:     uuid.NewString(),
		TenantID:             tenantID,
		MappingKey:           mappingKey,
		AccountCode:          accountCode,
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: creator,
	}
}

// TestPgStore_AccountMappingLifecycle_DraftApproveActivate proves the
// ordinary path against real Postgres: a mapping starts DRAFT, is not
// resolvable by GetCurrentAccountMapping until it is ACTIVE, and activating
// a second mapping for the same key supersedes the first — the structural
// guarantee idx_account_mappings_active (migration 000029) gives ACC-02's
// "no fallback guessing" requirement.
func TestPgStore_AccountMappingLifecycle_DraftApproveActivate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acctA := newTestAccount(tenantID, "4000", "Revenue A")
	acctB := newTestAccount(tenantID, "4001", "Revenue B")
	if err := s.CreateAccount(ctx, acctA); err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	if err := s.CreateAccount(ctx, acctB); err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	m1 := newMappingDraft(tenantID, "REVENUE:WIDGETS", "4000", "author-1")
	if err := s.CreateMapping(ctx, m1); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if m1.Status != domain.AccountMappingStatusDraft {
		t.Fatalf("expected DRAFT, got %s", m1.Status)
	}

	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("a DRAFT mapping must not resolve, got %v", err)
	}

	approved, err := s.ApproveMapping(ctx, tenantID, m1.AccountMappingID, "approver-1")
	if err != nil {
		t.Fatalf("ApproveMapping: %v", err)
	}
	if approved.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("expected APPROVED, got %s", approved.Status)
	}

	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("an APPROVED mapping must not resolve either, got %v", err)
	}

	activated, err := s.ActivateMapping(ctx, tenantID, m1.AccountMappingID, time.Time{})
	if err != nil {
		t.Fatalf("ActivateMapping: %v", err)
	}
	if activated.Status != domain.AccountMappingStatusActive {
		t.Fatalf("expected ACTIVE, got %s", activated.Status)
	}

	resolved, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS")
	if err != nil {
		t.Fatalf("GetCurrentAccountMapping: %v", err)
	}
	if resolved.AccountCode != "4000" {
		t.Fatalf("expected resolution to 4000, got %s", resolved.AccountCode)
	}

	// A second mapping activated for the same key supersedes the first —
	// idx_account_mappings_active permits only one ACTIVE row per key, so
	// this also proves the index doesn't block the legitimate replacement.
	m2 := newMappingDraft(tenantID, "REVENUE:WIDGETS", "4001", "author-1")
	if err := s.CreateMapping(ctx, m2); err != nil {
		t.Fatalf("CreateMapping m2: %v", err)
	}
	if _, err := s.ApproveMapping(ctx, tenantID, m2.AccountMappingID, "approver-1"); err != nil {
		t.Fatalf("ApproveMapping m2: %v", err)
	}
	if _, err := s.ActivateMapping(ctx, tenantID, m2.AccountMappingID, time.Time{}); err != nil {
		t.Fatalf("ActivateMapping m2: %v", err)
	}

	resolved2, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS")
	if err != nil {
		t.Fatalf("GetCurrentAccountMapping after supersession: %v", err)
	}
	if resolved2.AccountCode != "4001" {
		t.Fatalf("expected resolution to 4001 after supersession, got %s", resolved2.AccountCode)
	}

	oldVersion, err := s.GetMappingByID(ctx, tenantID, m1.AccountMappingID)
	if err != nil {
		t.Fatalf("GetMappingByID m1: %v", err)
	}
	if oldVersion.Status != domain.AccountMappingStatusSuperseded || oldVersion.SupersededByMappingID == nil || *oldVersion.SupersededByMappingID != m2.AccountMappingID {
		t.Fatalf("expected m1 superseded by m2, got %+v", oldVersion)
	}
}

// TestPgStore_AccountMappingLifecycle_HighRiskMakerChecker proves ACC-02's
// SoD requirement: a mapping onto a control account cannot be approved by
// its own author, and SetAccountMapping (the one-step legacy path) must
// refuse a control-account target outright rather than let it bypass
// governance.
func TestPgStore_AccountMappingLifecycle_HighRiskMakerChecker(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	control := newTestAccount(tenantID, "1100", "AR Control")
	control.IsControlAccount = true
	if err := s.CreateAccount(ctx, control); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	legacy := &domain.AccountMapping{
		AccountMappingID:     uuid.NewString(),
		TenantID:             tenantID,
		MappingKey:           "SUBLEDGER:AR",
		AccountCode:          "1100",
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "author-1",
	}
	if err := s.SetAccountMapping(ctx, legacy); !errors.Is(err, domain.ErrMappingHighRiskRequiresGovernedLifecycle) {
		t.Fatalf("expected ErrMappingHighRiskRequiresGovernedLifecycle, got %v", err)
	}

	m := newMappingDraft(tenantID, "SUBLEDGER:AR", "1100", "author-1")
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if !m.IsHighRisk {
		t.Fatalf("expected IsHighRisk=true for a control-account mapping")
	}

	if _, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "author-1"); !errors.Is(err, domain.ErrMappingSelfApproval) {
		t.Fatalf("expected ErrMappingSelfApproval for self-approval of a high-risk mapping, got %v", err)
	}

	approved, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "approver-2")
	if err != nil {
		t.Fatalf("ApproveMapping by a different principal: %v", err)
	}
	if approved.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("expected APPROVED, got %s", approved.Status)
	}
}

// TestPgStore_AccountMappingLifecycle_SupersedeWithoutReplacement proves the
// direct-retirement negative path: an ACTIVE mapping can be superseded with
// nothing yet approved to replace it, and the key then resolves to nothing —
// the posting engine must fail closed rather than guess.
func TestPgStore_AccountMappingLifecycle_SupersedeWithoutReplacement(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "5000", "Expense")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	m := newMappingDraft(tenantID, "EXPENSE:TRAVEL", "5000", "author-1")
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "approver-1"); err != nil {
		t.Fatalf("ApproveMapping: %v", err)
	}
	if _, err := s.ActivateMapping(ctx, tenantID, m.AccountMappingID, time.Time{}); err != nil {
		t.Fatalf("ActivateMapping: %v", err)
	}

	if _, err := s.SupersedeMapping(ctx, tenantID, m.AccountMappingID, "approver-1"); err != nil {
		t.Fatalf("SupersedeMapping: %v", err)
	}
	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "EXPENSE:TRAVEL"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("expected ErrAccountMappingNotFound after direct supersede, got %v", err)
	}

	// A second supersede must be refused — it is no longer ACTIVE.
	if _, err := s.SupersedeMapping(ctx, tenantID, m.AccountMappingID, "approver-1"); !errors.Is(err, domain.ErrMappingNotActive) {
		t.Fatalf("expected ErrMappingNotActive on a repeat supersede, got %v", err)
	}
}

// TestPgStore_GetMappingAsOf_ReconstructsHistory proves the point-in-time
// read: a query before activation sees nothing, between activation and
// supersession sees the version that was in force then, and after
// supersession a later as_of sees the replacement — history is read from the
// rows themselves, not rewritten when a mapping is superseded.
func TestPgStore_GetMappingAsOf_ReconstructsHistory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acctA := newTestAccount(tenantID, "6000", "COGS A")
	acctB := newTestAccount(tenantID, "6001", "COGS B")
	if err := s.CreateAccount(ctx, acctA); err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	if err := s.CreateAccount(ctx, acctB); err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	before := time.Now().UTC().Add(-time.Hour)

	m1 := newMappingDraft(tenantID, "COGS:MAIN", "6000", "author-1")
	if err := s.CreateMapping(ctx, m1); err != nil {
		t.Fatalf("CreateMapping m1: %v", err)
	}
	if _, err := s.ApproveMapping(ctx, tenantID, m1.AccountMappingID, "approver-1"); err != nil {
		t.Fatalf("ApproveMapping m1: %v", err)
	}
	t1 := time.Now().UTC()
	if _, err := s.ActivateMapping(ctx, tenantID, m1.AccountMappingID, t1); err != nil {
		t.Fatalf("ActivateMapping m1: %v", err)
	}

	mid := time.Now().UTC()

	m2 := newMappingDraft(tenantID, "COGS:MAIN", "6001", "author-1")
	if err := s.CreateMapping(ctx, m2); err != nil {
		t.Fatalf("CreateMapping m2: %v", err)
	}
	if _, err := s.ApproveMapping(ctx, tenantID, m2.AccountMappingID, "approver-1"); err != nil {
		t.Fatalf("ApproveMapping m2: %v", err)
	}
	t2 := time.Now().UTC()
	if _, err := s.ActivateMapping(ctx, tenantID, m2.AccountMappingID, t2); err != nil {
		t.Fatalf("ActivateMapping m2: %v", err)
	}

	after := time.Now().UTC()

	if _, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", before); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("expected nothing resolvable before either activation, got %v", err)
	}
	atMid, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", mid)
	if err != nil || atMid.AccountCode != "6000" {
		t.Fatalf("expected 6000 to be in force at mid, got %+v (err %v)", atMid, err)
	}
	atAfter, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", after)
	if err != nil || atAfter.AccountCode != "6001" {
		t.Fatalf("expected 6001 to be in force after the replacement, got %+v (err %v)", atAfter, err)
	}
}
