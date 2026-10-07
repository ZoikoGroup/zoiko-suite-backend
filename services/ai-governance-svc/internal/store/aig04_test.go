package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/store"
)

// ZS-SVC-X-001 §7 — AIG-04 (Human Oversight, Output Disposition &
// Decision Boundary Service), against real Postgres as the
// NOSUPERUSER NOBYPASSRLS app role.

// createActiveUseCase builds and activates a use case at the given
// operational class, so AIG-04 tests can submit real outputs against
// it. A2/A3 need a resolved legal classification to activate (§4.5).
func createActiveUseCase(t *testing.T, s *store.PgStore, ctx context.Context, tenant, opClass string) *domain.AIUseCase {
	t.Helper()
	req := baseCreateUseCaseRequest(opClass)
	if opClass == "A2" || opClass == "A3" {
		req.LegalClassificationRef = "legal-ref-resolved"
	}
	uc, err := s.CreateUseCase(ctx, req, tenant, "creator-carl", "")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}
	a, err := s.StartAssessment(ctx, uc.UseCaseID, domain.StartAssessmentRequest{RightsImpact: "none"}, "creator-carl")
	if err != nil {
		t.Fatalf("start assessment: %v", err)
	}
	if _, err := s.DecideAssessment(ctx, a.AssessmentID, string(domain.AssessmentApproved), "reviewer-bob", ""); err != nil {
		t.Fatalf("approve assessment: %v", err)
	}
	activated, err := s.ActivateUseCase(ctx, uc.UseCaseID, domain.ActivateUseCaseRequest{})
	if err != nil {
		t.Fatalf("activate use case: %v", err)
	}
	return activated
}

func TestAIG04_CreateDisposition_A1_DraftAssistive(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if d.OversightClass != domain.OversightNone || d.DispositionState != domain.DispositionDraftAssistive {
		t.Fatalf("A1 disposition: %+v", d)
	}
}

func TestAIG04_CreateDisposition_A3_QualifiedReview(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A3")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if d.OversightClass != domain.OversightQualifiedReview || d.DispositionState != domain.DispositionReviewRequired {
		t.Fatalf("A3 disposition: %+v", d)
	}
}

// Caller-asserted dual control bumps A3's default O2 to O3 for the
// highest-impact slice of a regulated use case.
func TestAIG04_CreateDisposition_A3_DualControlFlag(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A3")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{
		UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1", RequireDualControl: true,
	}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if d.OversightClass != domain.OversightDualControl {
		t.Fatalf("expected O3, got %+v", d)
	}
}

// A validation failure routes straight to BLOCKED regardless of
// oversight class, and a BLOCKED disposition is never reviewable.
func TestAIG04_CreateDisposition_ValidationFailure_BlockedAndTerminal(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{
		UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1", ValidationFailureReason: "schema validation failed",
	}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if d.DispositionState != domain.DispositionBlocked {
		t.Fatalf("expected BLOCKED, got %+v", d)
	}

	_, err = s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "reviewer-eve")
	if !errors.Is(err, domain.ErrDispositionNotDecidable) {
		t.Fatalf("decide a BLOCKED disposition: %v", err)
	}
}

func TestAIG04_CreateDisposition_UseCaseNotActive_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)

	uc, err := s.CreateUseCase(ctx, baseCreateUseCaseRequest("A1"), tenant, "creator-carl", "")
	if err != nil {
		t.Fatalf("create use case: %v", err)
	}
	_, err = s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if !errors.Is(err, domain.ErrUseCaseNotActiveForOutput) {
		t.Fatalf("create disposition against DRAFT use case: %v", err)
	}
}

func TestAIG04_DecideDisposition_HappyPath_Accepted(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A2")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	decided, err := s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{
		Decision: string(domain.DispositionAccepted), Reason: "looks correct",
	}, "submitter-dan")
	if err != nil {
		t.Fatalf("decide disposition: %v", err)
	}
	if decided.DispositionState != domain.DispositionAccepted || decided.ReviewerPrincipalID != "submitter-dan" {
		t.Fatalf("decided disposition: %+v", decided)
	}
}

// §7.2 "no self-bypass": for O2/O3, the reviewer must differ from the
// principal who submitted the output.
func TestAIG04_DecideDisposition_O2_SelfBypassRefused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A3")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	_, err = s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "submitter-dan")
	if !errors.Is(err, domain.ErrReviewerCannotBeSubmitter) {
		t.Fatalf("self-bypass decide: %v", err)
	}
}

// O0/O1 (DRAFT_ASSISTIVE) have no self-bypass restriction — the
// identified user may accept/reject their own assistive output.
func TestAIG04_DecideDisposition_O1_SelfAcceptAllowed(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A2")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	decided, err := s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "submitter-dan")
	if err != nil {
		t.Fatalf("O1 self-accept should be allowed: %v", err)
	}
	if decided.DispositionState != domain.DispositionAccepted {
		t.Fatalf("decided: %+v", decided)
	}
}

// NP-37: a decision made within one second of submission is recorded
// as a signal, never blocked.
func TestAIG04_DecideDisposition_RapidDecisionFlag(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	decided, err := s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "submitter-dan")
	if err != nil {
		t.Fatalf("decide disposition: %v", err)
	}
	if !decided.RapidDecisionFlag {
		t.Fatalf("expected rapid_decision_flag true for an immediate decision: %+v", decided)
	}
	if decided.DispositionState != domain.DispositionAccepted {
		t.Fatalf("rapid decision must still be accepted, not blocked: %+v", decided)
	}
}

func TestAIG04_DecideDisposition_AlreadyDecided_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if _, err := s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "submitter-dan"); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	_, err = s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionRejected)}, "submitter-dan")
	if !errors.Is(err, domain.ErrDispositionNotDecidable) {
		t.Fatalf("second decide on already-decided disposition: %v", err)
	}
}

// Amendment/supersession: a decided disposition can be replaced by a
// fresh one (e.g. the output needs re-review after a material change),
// never rewritten in place.
func TestAIG04_SupersedeDisposition_HappyPath(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	if _, err := s.DecideDisposition(ctx, d.DispositionID, domain.DecideDispositionRequest{Decision: string(domain.DispositionAccepted)}, "submitter-dan"); err != nil {
		t.Fatalf("decide: %v", err)
	}

	newD, err := s.SupersedeDisposition(ctx, d.DispositionID, domain.SupersedeDispositionRequest{ExecutionRef: "exec-1-revised"}, "submitter-dan")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if newD.DispositionID == d.DispositionID {
		t.Fatalf("supersede must create a NEW disposition row")
	}

	old, err := s.GetDisposition(ctx, d.DispositionID)
	if err != nil {
		t.Fatalf("get old disposition: %v", err)
	}
	if old.DispositionState != domain.DispositionSuperseded || old.SupersededByDispositionID != newD.DispositionID {
		t.Fatalf("old disposition after supersede: %+v", old)
	}
}

func TestAIG04_SupersedeDisposition_UndecidedRefused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A3")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}
	_, err = s.SupersedeDisposition(ctx, d.DispositionID, domain.SupersedeDispositionRequest{ExecutionRef: "exec-2"}, "submitter-dan")
	if !errors.Is(err, domain.ErrDispositionNotSupersedable) {
		t.Fatalf("supersede an undecided (REVIEW_REQUIRED) disposition: %v", err)
	}
}

// Raw-trigger negative control: a BLOCKED disposition is immutable
// even against a direct UPDATE, not just through the store's own
// guard.
func TestAIG04_RawUpdate_BlockedDisposition_RejectedByTrigger(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{
		UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1", ValidationFailureReason: "unsafe content",
	}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}

	_, err = admin.Exec(context.Background(), `UPDATE output_dispositions SET disposition_state = 'DRAFT_ASSISTIVE' WHERE disposition_id = $1`, d.DispositionID)
	if err == nil {
		t.Fatal("a BLOCKED disposition must be immutable at the database level")
	}
}

func TestAIG04_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	tenant := uuid.NewString()
	ctx := ctxWithTenant(tenant)
	uc := createActiveUseCase(t, s, ctx, tenant, "A1")

	d, err := s.CreateDisposition(ctx, domain.CreateDispositionRequest{UseCaseID: uc.UseCaseID, ExecutionRef: "exec-1"}, "submitter-dan")
	if err != nil {
		t.Fatalf("create disposition: %v", err)
	}

	otherTenant := uuid.NewString()
	otherCtx := ctxWithTenant(otherTenant)
	_, err = s.GetDisposition(otherCtx, d.DispositionID)
	if !errors.Is(err, domain.ErrDispositionNotFound) {
		t.Fatalf("ISOLATION FAILURE: cross-tenant disposition read: %v", err)
	}
}
