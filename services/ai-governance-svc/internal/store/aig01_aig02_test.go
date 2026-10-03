package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/middleware"
	"zoiko.io/ai-governance-svc/internal/store"
)

// ZS-SVC-X-001 Wave 1 — AIG-01 (AI Use-Case, Risk & Impact Registry) and
// AIG-02 (Model, Provider & Capability Registry), against real
// Postgres as the NOSUPERUSER NOBYPASSRLS app role.

func baseCreateUseCaseRequest(operationalClass string) domain.CreateUseCaseRequest {
	return domain.CreateUseCaseRequest{
		Domain: "accounting", Purpose: "draft expense categorization suggestions",
		OutcomeType: "classification_suggestion", OperationalClass: operationalClass,
		OwnerPrincipalID: "owner-alice", BusinessOutcome: "faster expense coding",
		AutomationLevel: string(domain.AutomationLevelClassification),
		HumanRole:       domain.HumanRole{AccountablePrincipalID: "owner-alice", ReviewerPrincipalID: "reviewer-bob", CanReject: true},
	}
}

func ctxWithTenant(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

// Doc-named acceptance scenario NP-01: unknown use_case_id -> BLOCK
// AIG-001; the read must fail closed, not return zero-value data.
func TestAIG01_UnknownUseCaseID_Blocks(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	_, err := s.GetUseCase(ctx, uuid.NewString())
	if !errors.Is(err, domain.ErrUseCaseNotFound) {
		t.Fatalf("get unknown use case: %v", err)
	}
}

// Doc-named acceptance scenario NP-04: PDC legal classification
// unavailable (INDETERMINATE/empty) fails closed for a potentially
// high-impact (A2/A3/A4) use case at activation, even with an approved
// assessment.
func TestAIG01_LegalClassificationIndeterminate_BlocksActivation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	req := baseCreateUseCaseRequest("A3")
	// LegalClassificationRef deliberately left empty.
	uc, err := s.CreateUseCase(ctx, req, tenant, "creator-carl", "")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}

	if _, err := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{}, "creator-carl"); err != nil {
		t.Fatalf("start assessment: %v", err)
	}
	eff, err := s.GetEffectiveUseCaseControl(ctx, uc.UseCaseID)
	if err != nil {
		t.Fatalf("get effective control: %v", err)
	}
	if _, err := s.DecideAssessment(ctx, eff.LatestAssessment.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}

	_, err = s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{})
	if !errors.Is(err, domain.ErrLegalClassificationIndeterminate) {
		t.Fatalf("activate with indeterminate legal classification: %v", err)
	}
}

// Happy path: A1 (no legal-classification requirement) with an
// approved assessment activates cleanly.
func TestAIG01_HappyPath_ApproveAndActivate(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	uc, err := s.CreateUseCase(ctx, baseCreateUseCaseRequest("A1"), tenant, "creator-carl", "")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}
	if uc.LifecycleState != domain.UseCaseDraft {
		t.Fatalf("initial state: %+v", uc)
	}

	a, err := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{RightsImpact: "none"}, "creator-carl")
	if err != nil {
		t.Fatalf("start assessment: %v", err)
	}
	if a.Version != 1 || a.Decision != domain.AssessmentPending {
		t.Fatalf("assessment: %+v", a)
	}

	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}

	activated, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{})
	if err != nil {
		t.Fatalf("activate use case: %v", err)
	}
	if activated.LifecycleState != domain.UseCaseActive {
		t.Fatalf("activated use case: %+v", activated)
	}
}

// Doc-named acceptance scenario NP-02 (in spirit): a SUSPENDED use case
// stays blocked from activation — suspension is reachable only from
// ACTIVE/LIMITED, and once suspended, activation requires a full
// reassessment cycle again (cannot skip straight back to ACTIVE).
func TestAIG01_SuspendedUseCase_CannotActivateWithoutReassessment(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	uc, _ := s.CreateUseCase(ctx, baseCreateUseCaseRequest("A1"), tenant, "creator-carl", "")
	a, _ := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{}, "creator-carl")
	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}
	if _, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{}); err != nil {
		t.Fatalf("activate use case: %v", err)
	}

	suspended, err := s.SuspendUseCase(ctx, uc.UseCaseID, domain.SuspendUseCaseRequest{Reason: "incident containment"})
	if err != nil {
		t.Fatalf("suspend use case: %v", err)
	}
	if suspended.LifecycleState != domain.UseCaseSuspended {
		t.Fatalf("suspended use case: %+v", suspended)
	}

	// Cannot jump straight back to ACTIVE from SUSPENDED.
	if _, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{}); !errors.Is(err, domain.ErrUseCaseNotActivatable) {
		t.Fatalf("activate a suspended use case directly: %v", err)
	}
}

// Doc-named acceptance scenario NP-05 (in spirit): material change ->
// reassessment, never a silent downgrade. RequestReassessment moves an
// ACTIVE use case back to ASSESSING, which then requires a fresh
// approved assessment before any further activation.
func TestAIG01_MaterialChange_ForcesReassessment(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	uc, _ := s.CreateUseCase(ctx, baseCreateUseCaseRequest("A1"), tenant, "creator-carl", "")
	a, _ := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{}, "creator-carl")
	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}
	active, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{})
	if err != nil || active.LifecycleState != domain.UseCaseActive {
		t.Fatalf("precondition: use case must be ACTIVE, got %+v (err=%v)", active, err)
	}

	reassessing, err := s.RequestReassessment(ctx, uc.UseCaseID, domain.RequestReassessmentRequest{Reason: "model capability changed"})
	if err != nil {
		t.Fatalf("request reassessment: %v", err)
	}
	if reassessing.LifecycleState != domain.UseCaseAssessing {
		t.Fatalf("reassessing use case: %+v", reassessing)
	}

	if _, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{}); !errors.Is(err, domain.ErrUseCaseNotActivatable) {
		t.Fatalf("activate mid-reassessment: %v", err)
	}
}

// A rejected assessment is terminal for that use case's current cycle,
// and the assessment row itself is immutable once decided.
func TestAIG01_RejectedAssessment_IsImmutable(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	uc, _ := s.CreateUseCase(ctx, baseCreateUseCaseRequest("A2"), tenant, "creator-carl", "")
	a, _ := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{}, "creator-carl")

	rejected, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentRejected), "reviewer-bob", "insufficient mitigations")
	if err != nil {
		t.Fatalf("reject assessment: %v", err)
	}
	if rejected.Decision != domain.AssessmentRejected {
		t.Fatalf("rejected assessment: %+v", rejected)
	}

	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); !errors.Is(err, domain.ErrAssessmentNotPending) {
		t.Fatalf("re-decide a rejected assessment: %v", err)
	}

	uc2, err := s.GetUseCase(ctx, uc.UseCaseID)
	if err != nil || uc2.LifecycleState != domain.UseCaseRejected {
		t.Fatalf("use case after rejected assessment: %+v (err=%v)", uc2, err)
	}
}

// A4 (prohibited-disabled) can never be activated, regardless of
// assessment outcome.
func TestAIG01_A4OperationalClass_NeverActivatable(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	req := baseCreateUseCaseRequest("A4")
	req.LegalClassificationRef = "resolved-ok"
	uc, _ := s.CreateUseCase(ctx, req, tenant, "creator-carl", "")
	a, _ := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{}, "creator-carl")
	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}

	if _, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{}); !errors.Is(err, domain.ErrOperationalClassProhibited) {
		t.Fatalf("activate an A4 use case: %v", err)
	}
}

// CreateUseCase's client_request_id gives idempotent create semantics:
// a replay with the same key returns the original row, not a second one.
func TestAIG01_CreateUseCase_IdempotentReplay(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	req := baseCreateUseCaseRequest("A1")
	first, err := s.CreateUseCase(ctx, req, tenant, "creator-carl", "client-req-1")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}
	second, err := s.CreateUseCase(ctx, req, tenant, "creator-carl", "client-req-1")
	if err != nil {
		t.Fatalf("replay create use case: %v", err)
	}
	if first.UseCaseID != second.UseCaseID {
		t.Fatalf("replay created a second use case: first=%s second=%s", first.UseCaseID, second.UseCaseID)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS.
func TestAIG01_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenantA2 := uuid.NewString()
	tenantB2 := uuid.NewString()
	uc, err := s.CreateUseCase(ctxWithTenant(tenantA2), baseCreateUseCaseRequest("A1"), tenantA2, "creator-carl", "")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}

	if _, err := s.GetUseCase(ctxWithTenant(tenantB2), uc.UseCaseID); !errors.Is(err, domain.ErrUseCaseNotFound) {
		t.Fatalf("cross-tenant get use case: %v", err)
	}
}

// ── AIG-02 ───────────────────────────────────────────────────────────────────

func baseRegisterReleaseRequest() domain.RegisterModelReleaseRequest {
	return domain.RegisterModelReleaseRequest{
		Provider: "anthropic", ProviderModelID: "claude-x", DeploymentRegion: "us-east",
		CapabilitySet: []string{"text"}, TrainingUse: string(domain.TrainingUseNoTraining),
	}
}

func allGatesClearedRequest() domain.ApproveReleaseRequest {
	return domain.ApproveReleaseRequest{
		PrivacyContractCleared: true, ResidencyCleared: true, SecurityCleared: true,
		EvaluationCleared: true, ExplainabilityCleared: true, ContinuityCleared: true, LegalCleared: true,
	}
}

// Happy path: a release moves DISCOVERED -> DUE_DILIGENCE ->
// EVALUATING -> APPROVED -> ACTIVE.
func TestAIG02_HappyPath_FullApprovalPipeline(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	m, err := s.RegisterModelRelease(ctx, baseRegisterReleaseRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("register model release: %v", err)
	}
	if m.ReleaseState != domain.ReleaseDiscovered {
		t.Fatalf("initial release state: %+v", m)
	}

	m, err = s.RecordDueDiligence(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{ControlEvidence: map[string]interface{}{"dpa": "signed"}}, "operator-dave")
	if err != nil || m.ReleaseState != domain.ReleaseDueDiligence {
		t.Fatalf("due diligence: %+v (err=%v)", m, err)
	}

	m, err = s.RecordEvaluation(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil || m.ReleaseState != domain.ReleaseEvaluating {
		t.Fatalf("evaluation: %+v (err=%v)", m, err)
	}

	m, err = s.ApproveRelease(ctx, m.ModelReleaseID, allGatesClearedRequest(), "operator-dave")
	if err != nil || m.ReleaseState != domain.ReleaseApproved {
		t.Fatalf("approve: %+v (err=%v)", m, err)
	}

	m, err = s.ActivateRelease(ctx, m.ModelReleaseID, "operator-dave")
	if err != nil || m.ReleaseState != domain.ReleaseActive {
		t.Fatalf("activate: %+v (err=%v)", m, err)
	}
}

// §5.4's procurement/enablement gate: approval is refused outright
// unless every one of the seven gates is cleared — no partial approval.
func TestAIG02_ApproveRelease_RequiresAllGatesCleared(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

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
		t.Fatalf("evaluation: %v", err)
	}

	partial := allGatesClearedRequest()
	partial.LegalCleared = false
	if _, err := s.ApproveRelease(ctx, m.ModelReleaseID, partial, "operator-dave"); !errors.Is(err, domain.ErrReleaseGatesNotCleared) {
		t.Fatalf("approve with one gate uncleared: %v", err)
	}

	reloaded, err := s.GetModelRelease(ctx, m.ModelReleaseID)
	if err != nil || reloaded.ReleaseState != domain.ReleaseEvaluating {
		t.Fatalf("release state after refused approval: %+v (err=%v)", reloaded, err)
	}
}

// Doc-named acceptance scenario NP-11: provider alias points to a new
// model silently -> resolve as quarantine/new release, never treat as
// the same release. Modeled here as: registering the "changed" model
// always creates a brand NEW immutable release row, never mutates the
// original.
func TestAIG02_ProviderAliasChange_NeverMutatesExistingRelease(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	original, err := s.RegisterModelRelease(ctx, baseRegisterReleaseRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("register original release: %v", err)
	}

	changedReq := baseRegisterReleaseRequest()
	changedReq.CapabilitySet = []string{"text", "vision"}
	changed, err := s.RegisterModelRelease(ctx, changedReq, "operator-dave")
	if err != nil {
		t.Fatalf("register changed release: %v", err)
	}

	if original.ModelReleaseID == changed.ModelReleaseID {
		t.Fatalf("provider alias change mutated the existing release instead of creating a new one")
	}

	reloadedOriginal, err := s.GetModelRelease(ctx, original.ModelReleaseID)
	if err != nil {
		t.Fatalf("reload original release: %v", err)
	}
	if len(reloadedOriginal.CapabilitySet) != 1 || reloadedOriginal.CapabilitySet[0] != "text" {
		t.Fatalf("original release's capability_set was mutated: %+v", reloadedOriginal)
	}
}

// Quarantine is reachable only from ACTIVE, and can only be retired
// from there — it deliberately cannot reactivate directly back to
// ACTIVE (that needs AIG-05's reactivation gate, a later wave).
func TestAIG02_Quarantine_CannotReactivateDirectly(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	m, err := s.RegisterModelRelease(ctx, baseRegisterReleaseRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("register release: %v", err)
	}
	m, err = s.RecordDueDiligence(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil {
		t.Fatalf("due diligence: %v", err)
	}
	m, err = s.RecordEvaluation(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil {
		t.Fatalf("evaluation: %v", err)
	}
	m, err = s.ApproveRelease(ctx, m.ModelReleaseID, allGatesClearedRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	m, err = s.ActivateRelease(ctx, m.ModelReleaseID, "operator-dave")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	quarantined, err := s.QuarantineRelease(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{Reason: "behavior drift detected"}, "operator-dave")
	if err != nil || quarantined.ReleaseState != domain.ReleaseQuarantined {
		t.Fatalf("quarantine release: %+v (err=%v)", quarantined, err)
	}

	if _, err := s.ActivateRelease(ctx, m.ModelReleaseID, "operator-dave"); !errors.Is(err, domain.ErrInvalidReleaseTransition) {
		t.Fatalf("reactivate a quarantined release directly: %v", err)
	}

	retired, err := s.RetireRelease(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{Reason: "decommissioned"}, "operator-dave")
	if err != nil || retired.ReleaseState != domain.ReleaseRetired {
		t.Fatalf("retire a quarantined release: %+v (err=%v)", retired, err)
	}

	_, err = admin.Exec(context.Background(), `UPDATE ai_model_releases SET release_state = 'ACTIVE' WHERE model_release_id = $1`, m.ModelReleaseID)
	if err == nil {
		t.Fatalf("reactivating a retired release via raw UPDATE should have been rejected by the trigger")
	}
}

// Rejecting a release in DUE_DILIGENCE is terminal.
func TestAIG02_RejectedRelease_IsTerminal(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	m, err := s.RegisterModelRelease(ctx, baseRegisterReleaseRequest(), "operator-dave")
	if err != nil {
		t.Fatalf("register release: %v", err)
	}
	m, err = s.RecordDueDiligence(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave")
	if err != nil {
		t.Fatalf("due diligence: %v", err)
	}

	rejected, err := s.RejectRelease(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{Reason: "DPA terms unacceptable"}, "operator-dave")
	if err != nil || rejected.ReleaseState != domain.ReleaseRejected {
		t.Fatalf("reject release: %+v (err=%v)", rejected, err)
	}

	if _, err := s.RecordEvaluation(ctx, m.ModelReleaseID, domain.AdvanceReleaseRequest{}, "operator-dave"); !errors.Is(err, domain.ErrInvalidReleaseTransition) {
		t.Fatalf("advance a rejected release: %v", err)
	}
}
