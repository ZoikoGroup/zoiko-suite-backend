package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
	"zoiko.io/project-accounting-svc/internal/store"
)

func newRecognitionReadyProject(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, legalEntityID, code string, estimateToComplete float64) string {
	t.Helper()
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, code)
	future := time.Now().UTC().Add(time.Hour)
	est := &domain.RecognitionEstimate{
		EstimateVersionID: uuid.New().String(), ProjectID: projectID, EstimateToComplete: estimateToComplete,
		EffectiveFrom: future, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "estimator-1",
	}
	if err := s.SetApprovedEstimate(ctx, est); err != nil {
		t.Fatalf("SetApprovedEstimate failed: %v", err)
	}
	return projectID
}

// TestPgStore_SetApprovedEstimate_Versions proves estimate changes are
// versioned (end-date + insert), never mutated in place — the real
// enforcement of "Progress estimate changed after approval without
// invalidation."
func TestPgStore_SetApprovedEstimate_Versions(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-1")

	future1 := time.Now().UTC().Add(time.Hour)
	v1 := &domain.RecognitionEstimate{
		EstimateVersionID: uuid.New().String(), ProjectID: projectID, EstimateToComplete: 100,
		EffectiveFrom: future1, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "estimator-1",
	}
	if err := s.SetApprovedEstimate(ctx, v1); err != nil {
		t.Fatalf("SetApprovedEstimate v1 failed: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("expected version 1, got %d", v1.Version)
	}

	future2 := future1.Add(time.Hour)
	v2 := &domain.RecognitionEstimate{
		EstimateVersionID: uuid.New().String(), ProjectID: projectID, EstimateToComplete: 50,
		EffectiveFrom: future2, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "estimator-1",
	}
	if err := s.SetApprovedEstimate(ctx, v2); err != nil {
		t.Fatalf("SetApprovedEstimate v2 failed: %v", err)
	}
	if v2.Version != 2 || v2.EstimateID != v1.EstimateID {
		t.Fatalf("expected v2 to be version 2 of the same logical estimate, got version=%d estimate_id=%s (v1=%s)", v2.Version, v2.EstimateID, v1.EstimateID)
	}

	// GetCurrentEstimate mirrors GetCurrentFinancialProfile's own
	// semantics: it returns the latest version (effective_to IS NULL),
	// not "as-of now" — the same convention proven by
	// TestPgStore_AmendFinancialProfile_Versions.
	current, err := s.GetCurrentEstimate(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentEstimate failed: %v", err)
	}
	if current.EstimateToComplete != 50 || current.Version != 2 {
		t.Fatalf("expected the latest version (v2, 50), got %+v", current)
	}
}

// TestPgStore_CreateRecognitionRun_DuplicatePeriod_Refused is the real
// proof of the partial UNIQUE index enforcing negative path #4, "no
// duplicate live recognition run for the same project+fiscal_period."
func TestPgStore_CreateRecognitionRun_DuplicatePeriod_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newRecognitionReadyProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-2", 100)

	contractValue := 1000.0
	first := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, first); err != nil {
		t.Fatalf("first CreateRecognitionRun failed: %v", err)
	}

	second := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, second); err != domain.ErrRecognitionRunAlreadyExistsForPeriod {
		t.Fatalf("expected ErrRecognitionRunAlreadyExistsForPeriod, got %v", err)
	}
}

// TestPgStore_FreezeAndCalculate_PercentageOfCompletion is the real proof
// of the cost-to-cost formula, computed against live project_cost_entries.
func TestPgStore_FreezeAndCalculate_PercentageOfCompletion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	// estimate_to_complete = 300; if ITD cost = 700, percent_complete = 700/(700+300) = 0.7
	projectID := newRecognitionReadyProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-3", 300)

	entry := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "cost-rec-1", 700)
	if err := s.CaptureProjectCost(ctx, entry); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	contractValue := 1000.0
	billed := 400.0
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue, BilledToDate: &billed,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}

	if err := s.FreezeAndCalculate(ctx, run.RunID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeAndCalculate failed: %v", err)
	}

	got, err := s.GetRecognitionRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRecognitionRun failed: %v", err)
	}
	if got.Status != domain.RecognitionRunStatusCalculated {
		t.Fatalf("expected CALCULATED, got %q", got.Status)
	}
	if got.PercentComplete == nil || *got.PercentComplete != 0.7 {
		t.Fatalf("expected percent_complete 0.7, got %v", got.PercentComplete)
	}
	if got.CumulativeRecognizedRevenue == nil || *got.CumulativeRecognizedRevenue != 700 {
		t.Fatalf("expected cumulative_recognized_revenue 700 (0.7*1000), got %v", got.CumulativeRecognizedRevenue)
	}
	if got.PeriodRecognizedRevenue == nil || *got.PeriodRecognizedRevenue != 700 {
		t.Fatalf("expected period_recognized_revenue 700 (no prior runs), got %v", got.PeriodRecognizedRevenue)
	}
	if got.BalanceAmount == nil || *got.BalanceAmount != 300 {
		t.Fatalf("expected balance_amount 300 (700 revenue - 400 billed), got %v", got.BalanceAmount)
	}
	if got.BalanceType == nil || *got.BalanceType != domain.BalanceTypeContractAsset {
		t.Fatalf("expected CONTRACT_ASSET (positive balance), got %v", got.BalanceType)
	}
}

// TestPgStore_FreezeAndCalculate_NoApprovedEstimate_Refused is the real
// proof a run cannot calculate without an approved estimate assigned.
func TestPgStore_FreezeAndCalculate_NoApprovedEstimate_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-4")

	contractValue := 1000.0
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}

	if err := s.FreezeAndCalculate(ctx, run.RunID, time.Now().UTC()); err != domain.ErrApprovedEstimateRequired {
		t.Fatalf("expected ErrApprovedEstimateRequired, got %v", err)
	}
}

// TestPgStore_RecognitionRun_CalculatedFieldsImmutable is the real proof
// of the reject-mutation trigger — once a run leaves DRAFT/
// POPULATION_FROZEN, its own calculated figures can never be edited
// in place, only superseded by a new run.
func TestPgStore_RecognitionRun_CalculatedFieldsImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newRecognitionReadyProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-5", 100)

	contractValue := 1000.0
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}
	if err := s.FreezeAndCalculate(ctx, run.RunID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeAndCalculate failed: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE project_recognition_runs SET margin = 999999 WHERE run_id = $1`, run.RunID); err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse mutating margin after CALCULATED, got no error")
	}
}

// TestPgStore_RecognitionRun_FullLifecycle exercises
// Draft -> Calculated -> Reviewed -> Approved -> AccountingEventEmitted -> Superseded.
func TestPgStore_RecognitionRun_FullLifecycle(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newRecognitionReadyProject(t, s, ctx, tenantID, legalEntityID, "PRJ-REC-6", 100)

	contractValue := 1000.0
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.FreezeAndCalculate(ctx, run.RunID, now); err != nil {
		t.Fatalf("FreezeAndCalculate failed: %v", err)
	}
	if err := s.ValidateRecognitionRun(ctx, run.RunID, now); err != nil {
		t.Fatalf("ValidateRecognitionRun failed: %v", err)
	}
	if err := s.ApproveRecognitionRun(ctx, run.RunID, "approver-1", now); err != nil {
		t.Fatalf("ApproveRecognitionRun failed: %v", err)
	}
	if err := s.MarkRecognitionRunEmitted(ctx, run.RunID, "journal-1", now); err != nil {
		t.Fatalf("MarkRecognitionRunEmitted failed: %v", err)
	}
	if err := s.SupersedeRecognitionRun(ctx, run.RunID, "manager-1", now); err != nil {
		t.Fatalf("SupersedeRecognitionRun failed: %v", err)
	}

	got, err := s.GetRecognitionRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRecognitionRun failed: %v", err)
	}
	if got.Status != domain.RecognitionRunStatusSuperseded {
		t.Fatalf("expected SUPERSEDED, got %q", got.Status)
	}

	// The (project, fiscal_period) slot must be free again for a new run.
	replacement := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, replacement); err != nil {
		t.Fatalf("expected a new run for the same period to succeed after supersession, got %v", err)
	}
}
