package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/store"
)

// ZS-SVC-X-001 §8 — AIG-05 (Evaluation, Monitoring, Incident & Change
// Governance Service), against real Postgres as the NOSUPERUSER
// NOBYPASSRLS app role. ai_model_releases/ai_evaluations/ai_incidents
// are all platform-wide (no tenant_id), so these tests use
// context.Background() directly, same as the existing AIG-02 tests.

func createActiveModelRelease(t *testing.T, s *store.PgStore, ctx context.Context) *domain.AIModelRelease {
	t.Helper()
	m, err := s.RegisterModelRelease(ctx, baseRegisterReleaseRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("register model release: %v", err)
	}
	m, err = s.RecordDueDiligence(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil {
		t.Fatalf("due diligence: %v", err)
	}
	m, err = s.RecordEvaluation(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil {
		t.Fatalf("record evaluation: %v", err)
	}
	m, err = s.ApproveRelease(ctx, m.ModelReleaseID, allGatesClearedRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("approve release: %v", err)
	}
	m, err = s.ActivateRelease(ctx, m.ModelReleaseID, "operator-dave")
	if err != nil {
		t.Fatalf("activate release: %v", err)
	}
	return m
}

func baseEvaluationRequest(modelReleaseID string, result domain.EvaluationResult) domain.CreateEvaluationRequest {
	return domain.CreateEvaluationRequest{
		ModelReleaseID: modelReleaseID, Dimension: string(domain.DimensionSafety),
		DatasetVersion: "golden-set-v3", Result: string(result),
		Metrics: map[string]interface{}{"injection_block_rate": 0.98},
	}
}

func TestAIG05_CreateEvaluation_HappyPath(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	e, err := s.CreateEvaluation(ctx, baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass), "evaluator-fay")
	if err != nil {
		t.Fatalf("create evaluation: %v", err)
	}
	if e.Dimension != domain.DimensionSafety || e.Result != domain.EvaluationPass {
		t.Fatalf("evaluation: %+v", e)
	}
}

func TestAIG05_CreateEvaluation_InvalidDimension_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	req := baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass)
	req.Dimension = "NOT_REAL"
	if _, err := s.CreateEvaluation(ctx, req, "evaluator-fay"); !errors.Is(err, domain.ErrInvalidEvaluationDimension) {
		t.Fatalf("invalid dimension: %v", err)
	}
}

func TestAIG05_CreateEvaluation_UnknownRelease_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	req := baseEvaluationRequest("00000000-0000-0000-0000-000000000000", domain.EvaluationPass)
	if _, err := s.CreateEvaluation(ctx, req, "evaluator-fay"); !errors.Is(err, domain.ErrModelReleaseNotFound) {
		t.Fatalf("unknown release: %v", err)
	}
}

// Doc's "immediate kill switch": an AI-P0 incident against an ACTIVE
// release quarantines it in the same transaction as the incident is
// filed — not a follow-up step.
func TestAIG05_ReportIncident_AIP0_AutoQuarantinesRelease(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP0), ModelReleaseID: m.ModelReleaseID,
		Description: "cross-tenant retrieval canary fired",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	if in.Severity != domain.SeverityAIP0 {
		t.Fatalf("incident: %+v", in)
	}

	reloaded, err := s.GetModelRelease(ctx, m.ModelReleaseID)
	if err != nil {
		t.Fatalf("get model release: %v", err)
	}
	if reloaded.ReleaseState != domain.ReleaseQuarantined {
		t.Fatalf("expected release auto-quarantined, got %+v", reloaded)
	}
}

// AI-P1 restricts rather than quarantines.
func TestAIG05_ReportIncident_AIP1_AutoRestrictsRelease(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	_, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP1), ModelReleaseID: m.ModelReleaseID,
		Description: "material wrong outputs at scale",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	reloaded, err := s.GetModelRelease(ctx, m.ModelReleaseID)
	if err != nil {
		t.Fatalf("get model release: %v", err)
	}
	if reloaded.ReleaseState != domain.ReleaseRestricted {
		t.Fatalf("expected release auto-restricted, got %+v", reloaded)
	}
}

// AI-P2/P3 have no automatic release-state consequence.
func TestAIG05_ReportIncident_AIP3_NoReleaseStateChange(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	_, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP3), ModelReleaseID: m.ModelReleaseID,
		Description: "minor non-material defect",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	reloaded, err := s.GetModelRelease(ctx, m.ModelReleaseID)
	if err != nil {
		t.Fatalf("get model release: %v", err)
	}
	if reloaded.ReleaseState != domain.ReleaseActive {
		t.Fatalf("AI-P3 must not change release state, got %+v", reloaded)
	}
}

func TestAIG05_ReportIncident_MissingScope_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	_, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP2), Description: "no scope named",
	}, "monitor-bot")
	if !errors.Is(err, domain.ErrIncidentMissingScope) {
		t.Fatalf("missing scope: %v", err)
	}
}

// Full incident lifecycle: OPEN -> CONTAINED -> RESOLVED -> CLOSED,
// each requiring its own evidence field.
func TestAIG05_IncidentLifecycle_FullCycle(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP2), ModelReleaseID: m.ModelReleaseID, Description: "localized quality regression",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}

	contained, err := s.ContainIncident(ctx, in.IncidentID, domain.ContainIncidentRequest{ContainmentAction: "disabled affected prompt variant"})
	if err != nil {
		t.Fatalf("contain: %v", err)
	}
	if contained.IncidentState != domain.IncidentContained {
		t.Fatalf("contained: %+v", contained)
	}

	resolved, err := s.ResolveIncident(ctx, in.IncidentID, domain.ResolveIncidentRequest{
		RootCause: "prompt template regression in v12", CorrectiveActions: "reverted to v11, added regression test",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.IncidentState != domain.IncidentResolved {
		t.Fatalf("resolved: %+v", resolved)
	}

	closed, err := s.CloseIncident(ctx, in.IncidentID, domain.CloseIncidentRequest{ClosureEvidence: "regression test passing in CI"}, "closer-greg")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.IncidentState != domain.IncidentClosed {
		t.Fatalf("closed: %+v", closed)
	}
}

func TestAIG05_IncidentLifecycle_CannotSkipContained(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP2), ModelReleaseID: m.ModelReleaseID, Description: "x",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	_, err = s.ResolveIncident(ctx, in.IncidentID, domain.ResolveIncidentRequest{RootCause: "x", CorrectiveActions: "x"})
	if !errors.Is(err, domain.ErrIncidentNotContained) {
		t.Fatalf("resolve an OPEN incident: %v", err)
	}
}

// Core reactivation gate: QUARANTINED -> ACTIVE requires a
// RESOLVED/CLOSED incident naming this release with a root cause, AND
// a PASS evaluation recorded AFTER that incident — not just any old
// passing evaluation.
func TestAIG05_ReactivateRelease_FullGate_HappyPath(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP0), ModelReleaseID: m.ModelReleaseID, Description: "unauthorized tool side effect",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	reloaded, err := s.GetModelRelease(ctx, m.ModelReleaseID)
	if err != nil || reloaded.ReleaseState != domain.ReleaseQuarantined {
		t.Fatalf("expected quarantined after AI-P0: %+v %v", reloaded, err)
	}

	if _, err := s.ContainIncident(ctx, in.IncidentID, domain.ContainIncidentRequest{ContainmentAction: "tool disabled"}); err != nil {
		t.Fatalf("contain: %v", err)
	}
	if _, err := s.ResolveIncident(ctx, in.IncidentID, domain.ResolveIncidentRequest{
		RootCause: "tool policy misconfigured", CorrectiveActions: "tightened tool allowlist",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Re-evaluation AFTER the incident was reported.
	if _, err := s.CreateEvaluation(ctx, baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass), "evaluator-fay"); err != nil {
		t.Fatalf("create re-evaluation: %v", err)
	}

	reactivated, err := s.ReactivateRelease(ctx, m.ModelReleaseID, domain.ReactivateReleaseRequest{
		IncidentID: in.IncidentID, Reason: "root cause fixed and re-evaluated",
	}, "approver-heidi")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if reactivated.ReleaseState != domain.ReleaseActive {
		t.Fatalf("reactivated: %+v", reactivated)
	}
}

// A stale evaluation recorded BEFORE the incident does not satisfy the
// gate — the corrective change must actually be re-evaluated.
func TestAIG05_ReactivateRelease_StaleEvaluation_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	// Evaluation recorded BEFORE the incident.
	if _, err := s.CreateEvaluation(ctx, baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass), "evaluator-fay"); err != nil {
		t.Fatalf("create pre-incident evaluation: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP0), ModelReleaseID: m.ModelReleaseID, Description: "x",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	if _, err := s.ContainIncident(ctx, in.IncidentID, domain.ContainIncidentRequest{ContainmentAction: "x"}); err != nil {
		t.Fatalf("contain: %v", err)
	}
	if _, err := s.ResolveIncident(ctx, in.IncidentID, domain.ResolveIncidentRequest{RootCause: "x", CorrectiveActions: "x"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, err = s.ReactivateRelease(ctx, m.ModelReleaseID, domain.ReactivateReleaseRequest{IncidentID: in.IncidentID}, "approver-heidi")
	if !errors.Is(err, domain.ErrReactivationNoPassingReevaluation) {
		t.Fatalf("reactivate with only a stale pre-incident evaluation: %v", err)
	}
}

func TestAIG05_ReactivateRelease_IncidentNotResolved_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP0), ModelReleaseID: m.ModelReleaseID, Description: "x",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	if _, err := s.CreateEvaluation(ctx, baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass), "evaluator-fay"); err != nil {
		t.Fatalf("create evaluation: %v", err)
	}

	_, err = s.ReactivateRelease(ctx, m.ModelReleaseID, domain.ReactivateReleaseRequest{IncidentID: in.IncidentID}, "approver-heidi")
	if !errors.Is(err, domain.ErrReactivationIncidentNotClosedOrResolved) {
		t.Fatalf("reactivate with an OPEN incident: %v", err)
	}
}

func TestAIG05_ReactivateRelease_NotQuarantined_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	_, err := s.ReactivateRelease(ctx, m.ModelReleaseID, domain.ReactivateReleaseRequest{IncidentID: "00000000-0000-0000-0000-000000000000"}, "approver-heidi")
	if !errors.Is(err, domain.ErrInvalidReleaseTransition) {
		t.Fatalf("reactivate an ACTIVE (non-quarantined) release: %v", err)
	}
}

// Raw-trigger negative control: a CLOSED incident is immutable even
// against a direct UPDATE.
func TestAIG05_RawUpdate_ClosedIncident_RejectedByTrigger(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	in, err := s.ReportIncident(ctx, domain.ReportIncidentRequest{
		Severity: string(domain.SeverityAIP3), ModelReleaseID: m.ModelReleaseID, Description: "x",
	}, "monitor-bot")
	if err != nil {
		t.Fatalf("report incident: %v", err)
	}
	if _, err := s.ContainIncident(ctx, in.IncidentID, domain.ContainIncidentRequest{ContainmentAction: "x"}); err != nil {
		t.Fatalf("contain: %v", err)
	}
	if _, err := s.ResolveIncident(ctx, in.IncidentID, domain.ResolveIncidentRequest{RootCause: "x", CorrectiveActions: "x"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.CloseIncident(ctx, in.IncidentID, domain.CloseIncidentRequest{ClosureEvidence: "x"}, "closer-greg"); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = admin.Exec(context.Background(), `UPDATE ai_incidents SET incident_state = 'OPEN' WHERE incident_id = $1`, in.IncidentID)
	if err == nil {
		t.Fatal("a CLOSED incident must be immutable at the database level")
	}
}

// Raw-trigger negative control: an evaluation row is immutable.
func TestAIG05_RawUpdate_Evaluation_RejectedByTrigger(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()
	m := createActiveModelRelease(t, s, ctx)

	e, err := s.CreateEvaluation(ctx, baseEvaluationRequest(m.ModelReleaseID, domain.EvaluationPass), "evaluator-fay")
	if err != nil {
		t.Fatalf("create evaluation: %v", err)
	}

	_, err = admin.Exec(context.Background(), `UPDATE ai_evaluations SET result = 'FAIL' WHERE evaluation_id = $1`, e.EvaluationID)
	if err == nil {
		t.Fatal("ai_evaluations rows must be immutable at the database level")
	}
}
