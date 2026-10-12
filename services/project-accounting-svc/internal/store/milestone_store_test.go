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

// milestoneFixture is an ACTIVE project whose current financial profile is
// MILESTONE. Deliberately NO approved estimate — the milestone method must
// not need one.
type milestoneFixture struct {
	s         *store.PgStore
	ctx       context.Context
	tenantID  string
	legalID   string
	projectID string
}

func setupMilestoneProject(t *testing.T, code string, method string) (*milestoneFixture, func(sql string, args ...any) error) {
	t.Helper()
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.New().String()
	legalID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalID, code)
	now := time.Now().UTC()
	prof := &domain.FinancialProfile{
		ProfileVersionID: uuid.New().String(), ProjectID: projectID, RecognitionMethod: method,
		BillingType: domain.BillingTypeFixedPrice, Currency: "USD", EffectiveFrom: now.Add(time.Hour), CreatedAt: now, CreatedByPrincipalID: "manager-1",
	}
	if err := s.AmendFinancialProfile(ctx, projectID, prof); err != nil {
		t.Fatalf("AmendFinancialProfile failed: %v", err)
	}
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(context.Background(), sql, args...)
		return err
	}
	return &milestoneFixture{s: s, ctx: ctx, tenantID: tenantID, legalID: legalID, projectID: projectID}, exec
}

func (f *milestoneFixture) define(t *testing.T, name string, amount float64) string {
	t.Helper()
	m := &domain.Milestone{MilestoneID: uuid.New().String(), ProjectID: f.projectID, Name: name, Amount: amount, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "pm-1"}
	if err := f.s.DefineMilestone(f.ctx, m); err != nil {
		t.Fatalf("DefineMilestone(%s) failed: %v", name, err)
	}
	return m.MilestoneID
}

func (f *milestoneFixture) achieveAndApprove(t *testing.T, id string) {
	t.Helper()
	now := time.Now().UTC()
	if err := f.s.MarkMilestoneAchieved(f.ctx, id, "pm-1", "acceptance-cert-1", now); err != nil {
		t.Fatalf("MarkMilestoneAchieved failed: %v", err)
	}
	if err := f.s.ApproveMilestoneAchievement(f.ctx, id, "approver-1", now); err != nil {
		t.Fatalf("ApproveMilestoneAchievement failed: %v", err)
	}
}

func (f *milestoneFixture) newRun(t *testing.T, period string, contract, billed float64) string {
	t.Helper()
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: f.legalID, ProjectID: f.projectID, FiscalPeriod: period,
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contract, BilledToDate: &billed,
		RevenueAccountCode: "4000", WIPAccountCode: "1300", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := f.s.CreateRecognitionRun(f.ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}
	return run.RunID
}

func f64(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

// TestMilestone_Recognition_OnlyApprovedAchievedCount proves the milestone
// method: revenue = approved+achieved milestones only; unachieved and
// achieved-but-unapproved contribute 0; billed_to_date (900, larger than the
// revenue) never becomes revenue; no estimate-to-complete is needed; the
// evidence table lists exactly the included milestone.
func TestMilestone_Recognition_OnlyApprovedAchievedCount(t *testing.T) {
	f, _ := setupMilestoneProject(t, "PRJ-MS-1", domain.RecognitionMethodMilestone)

	m1 := f.define(t, "Design", 300)
	m2 := f.define(t, "Build", 200)
	f.define(t, "Ship", 100) // PLANNED
	f.achieveAndApprove(t, m1)
	if err := f.s.MarkMilestoneAchieved(f.ctx, m2, "pm-1", "build-report", time.Now().UTC()); err != nil { // achieved, NOT approved
		t.Fatalf("MarkMilestoneAchieved failed: %v", err)
	}
	if err := f.s.CaptureProjectCost(f.ctx, newDraftCostEntry(f.projectID, domain.CostSourceTypeAP, "ms-cost-1", 120)); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	runID := f.newRun(t, "2026-09", 1000, 900)
	if err := f.s.FreezeAndCalculate(f.ctx, runID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeAndCalculate (MILESTONE, no estimate) failed: %v", err)
	}
	got, err := f.s.GetRecognitionRun(f.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.RecognitionRunStatusCalculated {
		t.Fatalf("expected CALCULATED, got %s", got.Status)
	}
	if f64(got.CumulativeRecognizedRevenue) != 300 || f64(got.PeriodRecognizedRevenue) != 300 {
		t.Fatalf("expected cumulative=period=300 (only the approved milestone), got %v / %v", f64(got.CumulativeRecognizedRevenue), f64(got.PeriodRecognizedRevenue))
	}
	if f64(got.PercentComplete) != 0.3 {
		t.Fatalf("expected percent_complete 0.3, got %v", f64(got.PercentComplete))
	}
	if f64(got.RecognizedCost) != 120 || f64(got.Margin) != 180 {
		t.Fatalf("expected cost 120 / margin 180, got %v / %v", f64(got.RecognizedCost), f64(got.Margin))
	}
	// billed 900 vs revenue 300: balance -600 => CONTRACT_LIABILITY; billing did NOT become revenue.
	if f64(got.BalanceAmount) != -600 || got.BalanceType == nil || *got.BalanceType != domain.BalanceTypeContractLiability {
		t.Fatalf("expected -600 CONTRACT_LIABILITY, got %v %v", f64(got.BalanceAmount), got.BalanceType)
	}
	if got.EstimateToComplete != nil {
		t.Fatalf("milestone run must not record an estimate_to_complete, got %v", *got.EstimateToComplete)
	}
	ev, err := f.s.ListRunMilestones(f.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].MilestoneID != m1 || ev[0].Amount != 300 {
		t.Fatalf("evidence must list exactly the approved milestone, got %+v", ev)
	}
}

// TestMilestone_NoApprovedMilestones_ZeroRevenue: billing alone never
// becomes revenue.
func TestMilestone_NoApprovedMilestones_ZeroRevenue(t *testing.T) {
	f, _ := setupMilestoneProject(t, "PRJ-MS-2", domain.RecognitionMethodMilestone)
	f.define(t, "Design", 300)
	runID := f.newRun(t, "2026-09", 1000, 1000) // fully billed
	if err := f.s.FreezeAndCalculate(f.ctx, runID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeAndCalculate failed: %v", err)
	}
	got, _ := f.s.GetRecognitionRun(f.ctx, runID)
	if f64(got.CumulativeRecognizedRevenue) != 0 || f64(got.PeriodRecognizedRevenue) != 0 {
		t.Fatalf("expected 0 revenue with no approved milestones even though fully billed, got %v", f64(got.CumulativeRecognizedRevenue))
	}
}

// TestMilestone_ExceedsContractValue_Refused: approved total 1200 > contract
// 1000 refuses the calculation with the distinct error and leaves the run
// DRAFT with no evidence rows.
func TestMilestone_ExceedsContractValue_Refused(t *testing.T) {
	f, _ := setupMilestoneProject(t, "PRJ-MS-3", domain.RecognitionMethodMilestone)
	f.achieveAndApprove(t, f.define(t, "A", 700))
	f.achieveAndApprove(t, f.define(t, "B", 500))
	runID := f.newRun(t, "2026-09", 1000, 0)
	if err := f.s.FreezeAndCalculate(f.ctx, runID, time.Now().UTC()); err != domain.ErrMilestonesExceedContractValue {
		t.Fatalf("expected ErrMilestonesExceedContractValue, got %v", err)
	}
	got, _ := f.s.GetRecognitionRun(f.ctx, runID)
	if got.Status != domain.RecognitionRunStatusDraft {
		t.Fatalf("a refused calculation must leave the run DRAFT, got %s", got.Status)
	}
	if ev, _ := f.s.ListRunMilestones(f.ctx, runID); len(ev) != 0 {
		t.Fatalf("a refused calculation must record no evidence, got %d rows", len(ev))
	}
}

// TestMilestone_SelfApproval_And_Evidence_Refused covers the SoD and
// evidence guards at the store, plus the DB CHECK backstop.
func TestMilestone_SelfApproval_And_Evidence_Refused(t *testing.T) {
	f, exec := setupMilestoneProject(t, "PRJ-MS-4", domain.RecognitionMethodMilestone)
	id := f.define(t, "Design", 100)

	if err := f.s.MarkMilestoneAchieved(f.ctx, id, "pm-1", "   ", time.Now().UTC()); err != domain.ErrMilestoneEvidenceRequired {
		t.Fatalf("expected ErrMilestoneEvidenceRequired, got %v", err)
	}
	if err := f.s.ApproveMilestoneAchievement(f.ctx, id, "approver-1", time.Now().UTC()); err != domain.ErrInvalidMilestoneTransition {
		t.Fatalf("approving a PLANNED milestone: expected ErrInvalidMilestoneTransition, got %v", err)
	}
	if err := f.s.MarkMilestoneAchieved(f.ctx, id, "pm-1", "cert-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ApproveMilestoneAchievement(f.ctx, id, "pm-1", time.Now().UTC()); err != domain.ErrSelfApprovalNotPermittedMilestone {
		t.Fatalf("expected ErrSelfApprovalNotPermittedMilestone, got %v", err)
	}
	// DB-level backstop: even raw SQL cannot record a self-approval.
	if err := exec(`UPDATE project_milestones SET approved_at = now(), approved_by_principal_id = 'pm-1' WHERE milestone_id = $1`, id); err == nil {
		t.Fatalf("expected the chk_milestone_approval_sod CHECK to refuse a self-approval written directly")
	}
	// And raw SQL cannot achieve without evidence.
	id2 := f.define(t, "Build", 50)
	if err := exec(`UPDATE project_milestones SET status = 'ACHIEVED', achieved_at = now(), achieved_by_principal_id = 'pm-1' WHERE milestone_id = $1`, id2); err == nil {
		t.Fatalf("expected the CHECK to refuse ACHIEVED without evidence")
	}
	if err := f.s.DefineMilestone(f.ctx, &domain.Milestone{MilestoneID: uuid.New().String(), ProjectID: f.projectID, Name: "Design", Amount: 1, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "pm-1"}); err != domain.ErrDuplicateMilestoneName {
		t.Fatalf("expected ErrDuplicateMilestoneName, got %v", err)
	}
}

// TestMilestone_EconomicFieldsImmutableOnceAchieved proves the trigger.
func TestMilestone_EconomicFieldsImmutableOnceAchieved(t *testing.T) {
	f, exec := setupMilestoneProject(t, "PRJ-MS-5", domain.RecognitionMethodMilestone)
	id := f.define(t, "Design", 100)
	// Still PLANNED: amount may be corrected.
	if err := exec(`UPDATE project_milestones SET amount = 110 WHERE milestone_id = $1`, id); err != nil {
		t.Fatalf("a PLANNED milestone's amount should still be editable, got %v", err)
	}
	f.achieveAndApprove(t, id)
	for _, stmt := range []string{
		`UPDATE project_milestones SET amount = 999999 WHERE milestone_id = $1`,
		`UPDATE project_milestones SET name = 'renamed' WHERE milestone_id = $1`,
		`UPDATE project_milestones SET achievement_evidence_ref = 'forged' WHERE milestone_id = $1`,
		`UPDATE project_milestones SET status = 'PLANNED', achieved_at = NULL, achieved_by_principal_id = NULL, achievement_evidence_ref = NULL, approved_at = NULL, approved_by_principal_id = NULL WHERE milestone_id = $1`,
		`UPDATE project_milestones SET approved_by_principal_id = 'someone-else' WHERE milestone_id = $1`,
		`DELETE FROM project_milestones WHERE milestone_id = $1`,
	} {
		if err := exec(stmt, id); err == nil {
			t.Fatalf("expected the immutability trigger to refuse: %s", stmt)
		}
	}
	got, _ := f.s.GetMilestone(f.ctx, id)
	if got.Amount != 110 || got.Status != domain.MilestoneStatusAchieved || got.ApprovedAt == nil {
		t.Fatalf("milestone must be unchanged, got %+v", got)
	}
}

// TestMilestone_ApprovedAfterFreeze_DoesNotChangeEarlierRun proves a run is
// reconstructable and immutable: approving another milestone after the
// freeze leaves the frozen run and its evidence untouched, and only the NEXT
// run picks it up (period revenue = delta over the prior cumulative).
func TestMilestone_ApprovedAfterFreeze_DoesNotChangeEarlierRun(t *testing.T) {
	f, exec := setupMilestoneProject(t, "PRJ-MS-6", domain.RecognitionMethodMilestone)
	m1 := f.define(t, "Design", 300)
	m2 := f.define(t, "Build", 200)
	f.achieveAndApprove(t, m1)

	run1 := f.newRun(t, "2026-08", 1000, 0)
	if err := f.s.FreezeAndCalculate(f.ctx, run1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	f.achieveAndApprove(t, m2) // approved AFTER run1's freeze

	got1, _ := f.s.GetRecognitionRun(f.ctx, run1)
	if f64(got1.CumulativeRecognizedRevenue) != 300 || f64(got1.PercentComplete) != 0.3 {
		t.Fatalf("run1 must still show 300 after a later approval, got %v / %v", f64(got1.CumulativeRecognizedRevenue), f64(got1.PercentComplete))
	}
	if ev, _ := f.s.ListRunMilestones(f.ctx, run1); len(ev) != 1 || ev[0].MilestoneID != m1 {
		t.Fatalf("run1 evidence must still be exactly m1, got %+v", ev)
	}
	// The frozen run's figures cannot be rewritten to include it either.
	if err := exec(`UPDATE project_recognition_runs SET cumulative_recognized_revenue = 500 WHERE run_id = $1`, run1); err == nil {
		t.Fatalf("expected the run immutability trigger to refuse rewriting a frozen run")
	}
	// Evidence is insert-only.
	if err := exec(`DELETE FROM project_recognition_run_milestones WHERE run_id = $1`, run1); err == nil {
		t.Fatalf("expected the insert-only trigger to refuse DELETE on evidence")
	}
	if err := exec(`UPDATE project_recognition_run_milestones SET amount = 1 WHERE run_id = $1`, run1); err == nil {
		t.Fatalf("expected the insert-only trigger to refuse UPDATE on evidence")
	}

	// Run1 must be live (not superseded) for the baseline; a new period picks up m2.
	run2 := f.newRun(t, "2026-09", 1000, 0)
	if err := f.s.FreezeAndCalculate(f.ctx, run2, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got2, _ := f.s.GetRecognitionRun(f.ctx, run2)
	if f64(got2.CumulativeRecognizedRevenue) != 500 || f64(got2.PeriodRecognizedRevenue) != 200 {
		t.Fatalf("run2 expected cumulative 500 / period 200, got %v / %v", f64(got2.CumulativeRecognizedRevenue), f64(got2.PeriodRecognizedRevenue))
	}
	if ev, _ := f.s.ListRunMilestones(f.ctx, run2); len(ev) != 2 {
		t.Fatalf("run2 evidence must list both milestones, got %d", len(ev))
	}
}

// TestMilestone_PercentageOfCompletion_StillRequiresEstimate keeps the POC
// precondition: a POC project is untouched by milestones.
func TestMilestone_PercentageOfCompletion_StillRequiresEstimate(t *testing.T) {
	f, _ := setupMilestoneProject(t, "PRJ-MS-7", domain.RecognitionMethodPercentageOfCompletion)
	f.achieveAndApprove(t, f.define(t, "Design", 300))
	runID := f.newRun(t, "2026-09", 1000, 0)
	if err := f.s.FreezeAndCalculate(f.ctx, runID, time.Now().UTC()); err != domain.ErrApprovedEstimateRequired {
		t.Fatalf("POC without an estimate must still be refused with ErrApprovedEstimateRequired, got %v", err)
	}
}

// TestMilestone_TenantIsolation: another tenant cannot see or approve.
func TestMilestone_TenantIsolation(t *testing.T) {
	f, _ := setupMilestoneProject(t, "PRJ-MS-8", domain.RecognitionMethodMilestone)
	id := f.define(t, "Design", 100)
	other := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	if _, err := f.s.GetMilestone(other, id); err != domain.ErrMilestoneNotFound {
		t.Fatalf("expected not found across tenants, got %v", err)
	}
	if err := f.s.MarkMilestoneAchieved(other, id, "pm-1", "x", time.Now().UTC()); err != domain.ErrMilestoneNotFound {
		t.Fatalf("expected not found across tenants, got %v", err)
	}
}
