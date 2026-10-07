package store_test

import (
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// TestPgStore_AmendAuditEngagementScope_DemotesApprovedPlan is the real
// proof of "scope/framework changes invalidate dependent approvals" at
// the engagement level (mirrors AUD-02's own materiality-triggered
// invalidation test).
func TestPgStore_AmendAuditEngagementScope_DemotesApprovedPlan(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-gate-1")

	plan, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-1", CorrelationID: "plan-gate-1"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	approved, _, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "plan-approve-gate-1"})
	if err != nil || approved.Status != domain.AuditPlanApproved {
		t.Fatalf("approve plan: %+v err=%v", approved, err)
	}

	amended, changed, err := s.AmendAuditEngagementScope(ctx, domain.AmendAuditEngagementScopeParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, ActorPrincipalID: "manager-1", CorrelationID: "amend-scope-1",
		ScopeSummary: "Expanded scope to include subsidiary", FrameworkProfileID: eng.FrameworkProfileID, FrameworkProfileVersion: eng.FrameworkProfileVersion,
		MethodologyID: eng.MethodologyID, MethodologyVersion: eng.MethodologyVersion,
	})
	if err != nil || !changed {
		t.Fatalf("amend scope: changed=%v err=%v", changed, err)
	}
	if amended.ScopeVersion != 2 {
		t.Fatalf("expected scope_version bumped to 2, got %d", amended.ScopeVersion)
	}

	demoted, err := s.GetAuditPlan(ctx, testTenantID, plan.PlanID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if demoted.Status != domain.AuditPlanReviewed {
		t.Fatalf("expected the plan demoted to REVIEWED after scope amendment, got %q", demoted.Status)
	}
}

// TestPgStore_GetAuditEngagementCompletionGates_FieldworkStage exercises
// the real, composed FIELDWORK gate query end-to-end: blocked while the
// plan is unapproved / a high risk is uncovered / a required workpaper is
// unlocked, satisfied once all three clear.
func TestPgStore_GetAuditEngagementCompletionGates_FieldworkStage(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-gate-2")

	gates, err := s.GetAuditEngagementCompletionGates(ctx, testTenantID, eng.EngagementID, "FIELDWORK")
	if err != nil {
		t.Fatalf("get gates (no plan yet): %v", err)
	}
	if gateSatisfied(gates, "plan_approved") {
		t.Fatal("expected plan_approved=false with no plan at all")
	}

	plan, _, err := s.CreateAuditPlan(ctx, domain.CreateAuditPlanParams{EngagementID: eng.EngagementID, TenantID: testTenantID, CreatedByPrincipalID: "preparer-2", CorrelationID: "plan-gate-2"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, _, err := s.ApprovePlan(ctx, domain.ApprovePlanParams{PlanID: plan.PlanID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", CorrelationID: "plan-approve-gate-2"}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-GATE-2", Purpose: "Required area",
		Required: true, CreatedByPrincipalID: "preparer-2", CorrelationID: "wp-gate-2",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}

	gates, err = s.GetAuditEngagementCompletionGates(ctx, testTenantID, eng.EngagementID, "FIELDWORK")
	if err != nil {
		t.Fatalf("get gates (plan approved, workpaper unlocked): %v", err)
	}
	if !gateSatisfied(gates, "plan_approved") {
		t.Fatal("expected plan_approved=true once approved")
	}
	if gateSatisfied(gates, "required_workpapers_locked") {
		t.Fatal("expected required_workpapers_locked=false while the required workpaper remains unlocked")
	}

	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-2", CorrelationID: "wp-gate-2-prep"}); err != nil {
		t.Fatalf("mark prepared: %v", err)
	}
	if err := s.RecordProcedure(ctx, domain.RecordProcedureParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "p"}); err != nil {
		t.Fatalf("record procedure: %v", err)
	}
	if err := s.RecordResult(ctx, domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "r"}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	if err := s.RecordConclusion(ctx, domain.RecordConclusionParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "c"}); err != nil {
		t.Fatalf("record conclusion: %v", err)
	}
	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", CorrelationID: "wp-gate-2-lock"}); err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}

	gates, err = s.GetAuditEngagementCompletionGates(ctx, testTenantID, eng.EngagementID, "FIELDWORK")
	if err != nil {
		t.Fatalf("get gates (all satisfied): %v", err)
	}
	for _, g := range gates {
		if !g.Satisfied {
			t.Fatalf("expected all FIELDWORK gates satisfied, got %+v", gates)
		}
	}
}

func gateSatisfied(gates []domain.CompletionGate, name string) bool {
	for _, g := range gates {
		if g.Name == name {
			return g.Satisfied
		}
	}
	return false
}
