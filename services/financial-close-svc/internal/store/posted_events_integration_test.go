//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A12's ACC-09/ACC-10
// slice: MarkAllocationRunPosted and MarkFXRevaluationPosted now also
// enqueue AllocationPosted / RevaluationPosted to the transactional outbox,
// in the same transaction as the POSTED status write.
package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_MarkAllocationRunPosted_EnqueuesAllocationPostedEvent_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)
	now := time.Now().UTC()

	ruleVersionID := uuid.New().String()
	rule := &domain.AllocationRule{
		RuleVersionID: ruleVersionID, RuleID: ruleVersionID, Version: 1, LegalEntityID: "le-1", Name: "IT shared cost",
		SourceAccountCode: "5000-ITSharedCost", Status: domain.AllocationRuleStatusApproved, CreatedAt: now,
		CreatedByPrincipalID: "preparer-1", CurrencyCode: storeTestCurrency,
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
	}
	if _, err := s.CreateAllocationRule(ctx, rule, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}

	run := &domain.AllocationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", RuleID: rule.RuleID, RuleVersionID: ruleVersionID,
		FiscalPeriod: "2026-01", SourceAccountCode: "5000-ITSharedCost", SourceAmount: 1000,
		Status: domain.AllocationRunStatusCalculated, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateAllocationRun(ctx, run); err != nil {
		t.Fatalf("CreateAllocationRun: %v", err)
	}

	if err := s.MarkAllocationRunPosted(ctx, run, "journal-1", now, "corr-1", "executor-1"); err != nil {
		t.Fatalf("MarkAllocationRunPosted: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM eventing_outbox
		 WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
		tenantID, "com.zoikosuite.accounting.allocation-run.posted", run.RunID).Scan(&count); err != nil {
		t.Fatalf("query eventing_outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 allocation-run.posted event, got %d", count)
	}
}

func TestPgStore_MarkFXRevaluationPosted_EnqueuesRevaluationPostedEvent_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)
	now := time.Now().UTC()

	run := &domain.FXRevaluationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		FXGainLossAccountCode: "7100-FXGainLoss", Status: domain.FXRevaluationStatusReview,
		CreatedAt: now, CreatedByPrincipalID: "preparer-1", CurrencyCode: storeTestCurrency,
	}
	if _, err := s.CreateFXRevaluationRun(ctx, run, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateFXRevaluationRun: %v", err)
	}
	if _, err := s.ApproveFXRevaluationRun(ctx, run, "approver-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("ApproveFXRevaluationRun: %v", err)
	}

	if err := s.MarkFXRevaluationPosted(ctx, run.RunID, run.LegalEntityID, "journal-1", "poster-1", now, "corr-2"); err != nil {
		t.Fatalf("MarkFXRevaluationPosted: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM eventing_outbox
		 WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
		tenantID, "com.zoikosuite.accounting.fx-revaluation-run.posted", run.RunID).Scan(&count); err != nil {
		t.Fatalf("query eventing_outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 fx-revaluation-run.posted event, got %d", count)
	}
}
