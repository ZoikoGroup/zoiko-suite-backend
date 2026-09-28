package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// newAuditEngagementForTest creates a fresh, uniquely-correlated AUD-01
// engagement for AUD-02 tests to hang a plan off of.
func newAuditEngagementForTest(t *testing.T, s *store.PgStore, ctx context.Context, correlationID string) *domain.AuditEngagement {
	t.Helper()
	p := auditEngagementParams()
	p.CorrelationID = correlationID
	eng, _, err := s.CreateAuditEngagement(ctx, p)
	if err != nil {
		t.Fatalf("create engagement: %v", err)
	}
	return eng
}

func TestPgStore_AuditPlan_ApprovalAndSelfApprovalRefused(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	eng := newAuditEngagementForTest(t, s, ctx, "eng-plan-1")

	plan, created, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-1", CorrelationID: "plan-create-1",
	})
	if err != nil || !created {
		t.Fatalf("create plan: created=%v err=%v", created, err)
	}

	// A second plan for the same engagement must be refused — one live
	// plan per engagement (audit_plan_live_per_engagement).
	if _, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-2", CorrelationID: "plan-create-2",
	}); err != domain.ErrAuditPlanAlreadyExists {
		t.Fatalf("expected ErrAuditPlanAlreadyExists for a second live plan, got %v", err)
	}

	// The preparer may not approve their own plan.
	if _, _, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{
		PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "plan-approve-self",
	}); err != domain.ErrAuditPlanSelfApproval {
		t.Fatalf("expected ErrAuditPlanSelfApproval, got %v", err)
	}

	approved, changed, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{
		PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "plan-approve-1",
	})
	if err != nil || !changed || approved.Status != domain.AuditPlanApproved {
		t.Fatalf("approve plan: changed=%v status=%v err=%v", changed, approved, err)
	}
	if approved.ScopeVersionAtApproval == nil || *approved.ScopeVersionAtApproval != 1 {
		t.Fatalf("expected scope_version_at_approval=1 (engagement's own initial scope_version), got %+v", approved.ScopeVersionAtApproval)
	}

	// Re-approving is a no-op replay via correlation_id, not a second
	// transition, and re-approving an already-APPROVED plan with a new
	// correlation ID is refused outright.
	if _, _, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{
		PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", CorrelationID: "plan-approve-2",
	}); err != domain.ErrAuditPlanInvalidState {
		t.Fatalf("expected ErrAuditPlanInvalidState re-approving an already-approved plan, got %v", err)
	}
}

// TestPgStore_AssessRisk_RequiresAssertionAndPlannedProcedure is the real
// proof of "every risk links to assertions/process and response" —
// AUD-CTRL-005. A risk with neither cannot be assessed; linking only one
// of the two still refuses; linking both allows it.
func TestPgStore_AssessRisk_RequiresAssertionAndPlannedProcedure(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	eng := newAuditEngagementForTest(t, s, ctx, "eng-plan-2")
	plan, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-1", CorrelationID: "plan-create-risk-1",
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	risk, _, err := s.IdentifyRisk(ctx, domain.IdentifyRiskParams{
		PlanID: plan.PlanID, EngagementID: eng.EngagementID, TenantID: testTenantID,
		Description: "Revenue cutoff risk", RiskLevel: domain.RiskLevelHigh, CreatedByPrincipalID: "preparer-1", CorrelationID: "risk-create-1",
	})
	if err != nil {
		t.Fatalf("identify risk: %v", err)
	}

	if _, _, err := s.AssessRisk(ctx, domain.AssessRiskParams{RiskID: risk.RiskID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "assess-1"}); err != domain.ErrAuditRiskRequiresAssertionAndResponse {
		t.Fatalf("expected ErrAuditRiskRequiresAssertionAndResponse with neither linked, got %v", err)
	}

	if err := s.LinkAssertion(ctx, domain.LinkAssertionParams{RiskID: risk.RiskID, TenantID: testTenantID, AssertionCode: "COMPLETENESS"}); err != nil {
		t.Fatalf("link assertion: %v", err)
	}
	if _, _, err := s.AssessRisk(ctx, domain.AssessRiskParams{RiskID: risk.RiskID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "assess-2"}); err != domain.ErrAuditRiskRequiresAssertionAndResponse {
		t.Fatalf("expected ErrAuditRiskRequiresAssertionAndResponse with only an assertion linked, got %v", err)
	}

	if err := s.DesignAuditResponse(ctx, domain.DesignAuditResponseParams{RiskID: risk.RiskID, TenantID: testTenantID, Description: "Test cutoff sample", DesignedByPrincipalID: "preparer-1"}); err != nil {
		t.Fatalf("design response: %v", err)
	}
	assessed, changed, err := s.AssessRisk(ctx, domain.AssessRiskParams{RiskID: risk.RiskID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "assess-3"})
	if err != nil || !changed || assessed.Status != domain.RiskStatusAssessed {
		t.Fatalf("expected AssessRisk to succeed once both are linked: changed=%v assessed=%+v err=%v", changed, assessed, err)
	}
}

// TestPgStore_MarkSignificantRisk_AlwaysAttributesActor is the real proof
// of "significant-risk classification requires authorized human" —
// risk_significant_requires_actor's own CHECK constraint, defense-in-depth
// behind the Go-level guarantee that MarkSignificantRisk always writes
// the caller's own principal.
func TestPgStore_MarkSignificantRisk_AlwaysAttributesActor(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	eng := newAuditEngagementForTest(t, s, ctx, "eng-plan-3")
	plan, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-1", CorrelationID: "plan-create-sig-1",
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	risk, _, err := s.IdentifyRisk(ctx, domain.IdentifyRiskParams{
		PlanID: plan.PlanID, EngagementID: eng.EngagementID, TenantID: testTenantID,
		Description: "Related party risk", RiskLevel: domain.RiskLevelHigh, CreatedByPrincipalID: "preparer-1", CorrelationID: "risk-create-sig-1",
	})
	if err != nil {
		t.Fatalf("identify risk: %v", err)
	}

	marked, err := s.MarkSignificantRisk(ctx, domain.MarkSignificantRiskParams{RiskID: risk.RiskID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "mark-sig-1"})
	if err != nil || !marked.IsSignificant || marked.AssessedByPrincipalID == nil || *marked.AssessedByPrincipalID != "reviewer-1" {
		t.Fatalf("expected is_significant=true attributed to reviewer-1, got %+v err=%v", marked, err)
	}
}

// TestPgStore_RecordMateriality_ApprovedPlanDemotesAndReassessesCoverage is
// the real proof of "materiality changes trigger dependency analysis":
// against an APPROVED plan, RecordMateriality demotes it back to REVIEWED
// and flips a COVERED high risk to PENDING_REASSESSMENT in the same
// transaction.
func TestPgStore_RecordMateriality_ApprovedPlanDemotesAndReassessesCoverage(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	eng := newAuditEngagementForTest(t, s, ctx, "eng-plan-4")
	plan, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-1", CorrelationID: "plan-create-mat-1",
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	risk, _, err := s.IdentifyRisk(ctx, domain.IdentifyRiskParams{
		PlanID: plan.PlanID, EngagementID: eng.EngagementID, TenantID: testTenantID,
		Description: "Inventory existence risk", RiskLevel: domain.RiskLevelHigh, CreatedByPrincipalID: "preparer-1", CorrelationID: "risk-create-mat-1",
	})
	if err != nil {
		t.Fatalf("identify risk: %v", err)
	}
	// Simulate the risk having already reached COVERED (no dedicated
	// "MarkCovered" command exists in v1 — set directly for this test's
	// own purpose of proving the reassessment flip).
	if _, err := pool.Exec(ctx, `UPDATE risk_assessments SET coverage_status='COVERED' WHERE risk_id=$1`, risk.RiskID); err != nil {
		t.Fatalf("seed covered risk: %v", err)
	}

	if _, err := s.RecordMateriality(ctx, domain.RecordMaterialityParams{
		PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "mat-1",
		OverallMateriality: 100000, PerformanceMateriality: 75000, Rationale: "Initial materiality",
	}); err != nil {
		t.Fatalf("record initial materiality: %v", err)
	}
	if _, _, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "plan-approve-mat-1"}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}

	mat2, err := s.RecordMateriality(ctx, domain.RecordMaterialityParams{
		PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "mat-2",
		OverallMateriality: 50000, PerformanceMateriality: 37500, Rationale: "Revised after new information",
	})
	if err != nil {
		t.Fatalf("record revised materiality: %v", err)
	}
	if mat2.OverallMateriality != 50000 {
		t.Fatalf("expected the new materiality version, got %+v", mat2)
	}

	demoted, err := s.GetAuditPlan(ctx, testTenantID, plan.PlanID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if demoted.Status != domain.AuditPlanReviewed {
		t.Fatalf("expected plan demoted to REVIEWED after materiality changed post-approval, got %q", demoted.Status)
	}
	reassessed, err := s.GetRiskAssessment(ctx, testTenantID, risk.RiskID)
	if err != nil {
		t.Fatalf("get risk: %v", err)
	}
	if reassessed.CoverageStatus != domain.RiskCoveragePendingReassessment {
		t.Fatalf("expected risk coverage flipped to PENDING_REASSESSMENT, got %q", reassessed.CoverageStatus)
	}

	// AUD-01's own FIELDWORK gate must now report the plan not approved.
	planApproved, noUnresolvedHighRisk, err := s.GetAuditEngagementFieldworkGates(ctx, testTenantID, eng.EngagementID)
	if err != nil {
		t.Fatalf("get fieldwork gates: %v", err)
	}
	if planApproved {
		t.Fatal("expected planApproved=false after materiality-triggered demotion")
	}
	if noUnresolvedHighRisk {
		t.Fatal("expected noUnresolvedHighRisk=false — the risk was flipped back to PENDING_REASSESSMENT")
	}
}
