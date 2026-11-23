//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11's ACC-09 slice:
// ApproveAllocationRule (a transition claim, mirroring the ACC-07/08 tests)
// and SupersedeAllocationRule (a CREATE claim, since it mints a new version
// on every call — see its own doc comment in pg_store.go for why).
package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func draftAllocationRule(name string) *domain.AllocationRule {
	ruleID := uuid.New().String()
	return &domain.AllocationRule{
		CurrencyCode: storeTestCurrency,
		RuleVersionID: ruleID, RuleID: ruleID, Version: 1, LegalEntityID: "le-1", Name: name,
		SourceAccountCode: "5000-ITSharedCost",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 60},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 40},
		},
		Status: domain.AllocationRuleStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_ApproveAllocationRule_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	rule := draftAllocationRule("approve replay")
	if _, err := s.CreateAllocationRule(ctx, rule, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.ApproveAllocationRule(ctx, rule, "approver-1", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first approve: replay=%v err=%v", replay, err)
	}
	if rule.Status != domain.AllocationRuleStatusApproved {
		t.Fatalf("status after approve = %q", rule.Status)
	}

	retryRule := &domain.AllocationRule{RuleVersionID: rule.RuleVersionID}
	replay, err := s.ApproveAllocationRule(ctx, retryRule, "approver-1", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried approve: want replay, got replay=%v err=%v", replay, err)
	}
	if retryRule.Status != domain.AllocationRuleStatusApproved {
		t.Fatalf("replay status = %q, want APPROVED", retryRule.Status)
	}
	if len(retryRule.Drivers) != 2 {
		t.Fatalf("replay must also carry the drivers, got %+v", retryRule.Drivers)
	}
}

func TestPgStore_SupersedeAllocationRule_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	rule := draftAllocationRule("supersede replay")
	if _, err := s.CreateAllocationRule(ctx, rule, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}
	if _, err := s.ApproveAllocationRule(ctx, rule, "approver-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("approve: %v", err)
	}

	key := uuid.NewString()
	newVersion := &domain.AllocationRule{
		CurrencyCode: storeTestCurrency, RuleVersionID: uuid.New().String(), LegalEntityID: "le-1",
		Name: "revised", SourceAccountCode: "5000-ITSharedCost",
		Drivers:   []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	firstVersionID := newVersion.RuleVersionID
	if replay, err := s.SupersedeAllocationRule(ctx, rule.RuleID, newVersion, now, key, testRequestHash); err != nil || replay {
		t.Fatalf("first supersede: replay=%v err=%v", replay, err)
	}

	// The handler mints a FRESH RuleVersionID on every call, including a
	// retry — the whole point of the request-hash design (it must not
	// depend on this freshly-generated id). A retry's own fresh id must be
	// discarded in favor of the original.
	retryVersion := &domain.AllocationRule{
		CurrencyCode: storeTestCurrency, RuleVersionID: uuid.New().String(), LegalEntityID: "le-1",
		Name: "revised", SourceAccountCode: "5000-ITSharedCost",
		Drivers:   []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	replay, err := s.SupersedeAllocationRule(ctx, rule.RuleID, retryVersion, now, key, testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retried supersede: want replay, got replay=%v err=%v", replay, err)
	}
	if retryVersion.RuleVersionID != firstVersionID {
		t.Fatalf("replay returned version %s, want the original %s", retryVersion.RuleVersionID, firstVersionID)
	}

	// A genuinely new attempt (a different key) must be refused: the rule
	// now has no current APPROVED/ACTIVE version to supersede (the first
	// supersede's new version is still DRAFT).
	another := &domain.AllocationRule{
		CurrencyCode: storeTestCurrency, RuleVersionID: uuid.New().String(), LegalEntityID: "le-1",
		Name: "another", SourceAccountCode: "5000-ITSharedCost",
		Drivers:   []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.SupersedeAllocationRule(ctx, rule.RuleID, another, now, uuid.NewString(), testRequestHash); !errors.Is(err, domain.ErrNoCurrentRuleToSupersede) {
		t.Fatalf("new supersede attempt with no current version: want ErrNoCurrentRuleToSupersede, got %v", err)
	}
}
