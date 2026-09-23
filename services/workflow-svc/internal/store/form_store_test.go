package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

func newFormForTest(t *testing.T, s *store.PgStore, ctx context.Context, correlationID string, schema []string) *domain.FormDefinition {
	t.Helper()
	form, _, err := s.CreateForm(ctx, domain.CreateFormParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		Name: "Expense Claim", BusinessPurpose: "Employee expense reimbursement", TargetDomain: "EXPENSE_CLAIM",
		Schema: schema, OwnerPrincipalID: "owner-1", CorrelationID: correlationID,
	})
	if err != nil {
		t.Fatalf("create form: %v", err)
	}
	return form
}

// TestPgStore_CreateForm_IdempotentOnCorrelationID proves a retried
// create resolves to the original form, never a duplicate.
func TestPgStore_CreateForm_IdempotentOnCorrelationID(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	first, created, err := s.CreateForm(ctx, domain.CreateFormParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		Name: "Expense Claim", BusinessPurpose: "Reimbursement", TargetDomain: "EXPENSE_CLAIM",
		Schema: []string{"amount"}, OwnerPrincipalID: "owner-1", CorrelationID: "form-create-1",
	})
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	retry, created, err := s.CreateForm(ctx, domain.CreateFormParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		Name: "Expense Claim", BusinessPurpose: "Reimbursement", TargetDomain: "EXPENSE_CLAIM",
		Schema: []string{"amount"}, OwnerPrincipalID: "owner-2", CorrelationID: "form-create-1",
	})
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if created {
		t.Fatal("a retry with the same correlation_id must not create a second form")
	}
	if retry.FormID != first.FormID {
		t.Fatalf("retry resolved to %s, want the original %s", retry.FormID, first.FormID)
	}
	if retry.OwnerPrincipalID != "owner-1" {
		t.Fatalf("expected the original owner to survive the replay, got %q", retry.OwnerPrincipalID)
	}
}

// TestPgStore_PublishForm_RejectsSelfPublish is the negative-controlled
// proof of the maker-checker requirement — every form, not only ones
// flagged sensitive (same resolution as BIZ-03's template approval
// scope).
func TestPgStore_PublishForm_RejectsSelfPublish(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-self-publish", nil)

	if _, err := s.PublishForm(ctx, domain.PublishFormParams{
		FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "owner-1", CorrelationID: "publish-self",
	}); err != domain.ErrFormSelfPublish {
		t.Fatalf("expected ErrFormSelfPublish, got %v", err)
	}

	// Negative control at the DB layer — the CHECK constraint refuses the
	// same self-publish even bypassing the store entirely.
	if _, err := pool.Exec(context.Background(),
		`UPDATE form_definitions SET status='PUBLISHED', published_by_principal_id='owner-1', published_at=now() WHERE form_id=$1`,
		form.FormID); err == nil {
		t.Fatal("expected the CHECK constraint to refuse a self-publish written directly")
	}
}

// TestPgStore_PublishForm_ThenRejectsRepublish proves PublishForm
// succeeds once with a different approver, and a second attempt is
// refused rather than silently re-applied.
func TestPgStore_PublishForm_ThenRejectsRepublish(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-publish-once", nil)

	published, err := s.PublishForm(ctx, domain.PublishFormParams{
		FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-1", CorrelationID: "publish-1",
	})
	if err != nil || published.Status != "PUBLISHED" {
		t.Fatalf("publish: status=%v err=%v", published, err)
	}

	if _, err := s.PublishForm(ctx, domain.PublishFormParams{
		FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-2", CorrelationID: "publish-2",
	}); err != domain.ErrFormNotDraft {
		t.Fatalf("expected ErrFormNotDraft on republish, got %v", err)
	}
}

// TestPgStore_RetireForm_BlocksNewDraftsThenRejectsDoubleRetire proves
// RetireForm's own effect (SaveDraft against a retired form is refused)
// and its own idempotency guard.
func TestPgStore_RetireForm_BlocksNewDraftsThenRejectsDoubleRetire(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-retire-1", nil)

	retired, err := s.RetireForm(ctx, domain.RetireFormParams{FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-1", CorrelationID: "retire-1"})
	if err != nil || retired.Status != "RETIRED" {
		t.Fatalf("retire: status=%v err=%v", retired, err)
	}

	if _, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-after-retire",
		SubmittedValues: map[string]string{"amount": "100"},
	}); err != domain.ErrFormRetired {
		t.Fatalf("expected ErrFormRetired, got %v", err)
	}

	if _, err := s.RetireForm(ctx, domain.RetireFormParams{FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-1", CorrelationID: "retire-2"}); err != domain.ErrFormAlreadyRetired {
		t.Fatalf("expected ErrFormAlreadyRetired, got %v", err)
	}
}

// TestPgStore_SaveDraft_ThenSubmit_ValuesBecomeImmutable proves the
// doc's own "immutable submissions" purpose line: SaveDraft may update
// values freely while DRAFT, and once SubmitForm concludes, a raw SQL
// attempt to change the values is refused by the trigger.
func TestPgStore_SaveDraft_ThenSubmit_ValuesBecomeImmutable(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-immutable-1", []string{"amount"})

	draft, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-1",
		SubmittedValues: map[string]string{"amount": "50"},
	})
	if err != nil {
		t.Fatalf("save draft: %v", err)
	}

	updated, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		SubmissionID: draft.SubmissionID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-2",
		SubmittedValues: map[string]string{"amount": "75"},
	})
	if err != nil || updated.SubmittedValues["amount"] != "75" {
		t.Fatalf("expected the second save to update the value, got %+v err=%v", updated, err)
	}

	submitted, err := s.SubmitForm(ctx, domain.SubmitFormParams{SubmissionID: draft.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "submitter-1", CorrelationID: "submit-1"})
	if err != nil || submitted.Status != domain.FormSubmissionSubmitted {
		t.Fatalf("submit: status=%v err=%v", submitted, err)
	}

	if _, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		SubmissionID: draft.SubmissionID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-after-submit",
		SubmittedValues: map[string]string{"amount": "999"},
	}); err != domain.ErrFormSubmissionNotDraft {
		t.Fatalf("expected ErrFormSubmissionNotDraft, got %v", err)
	}

	// Negative control at the DB layer.
	if _, err := pool.Exec(context.Background(),
		`UPDATE form_submissions SET submitted_values='{"amount":"999"}'::jsonb WHERE submission_id=$1`, draft.SubmissionID); err == nil {
		t.Fatal("expected the trigger to refuse mutating submitted_values after SUBMITTED")
	}
}

// TestPgStore_ValidateSubmission_AcceptsCompleteRejectsIncomplete proves
// the real required-field check against the form's schema.
func TestPgStore_ValidateSubmission_AcceptsCompleteRejectsIncomplete(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-validate-1", []string{"amount", "employee_id"})

	incomplete, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-incomplete",
		SubmittedValues: map[string]string{"amount": "50"},
	})
	if err != nil {
		t.Fatalf("save incomplete draft: %v", err)
	}
	if _, err := s.SubmitForm(ctx, domain.SubmitFormParams{SubmissionID: incomplete.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "submitter-1", CorrelationID: "submit-incomplete"}); err != nil {
		t.Fatalf("submit incomplete: %v", err)
	}
	rejected, err := s.ValidateSubmission(ctx, domain.ValidateSubmissionParams{SubmissionID: incomplete.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "validator-1", CorrelationID: "validate-incomplete"})
	if err != nil || rejected.Status != domain.FormSubmissionRejected {
		t.Fatalf("expected REJECTED for missing employee_id: status=%v err=%v", rejected, err)
	}
	if rejected.ValidationResult == nil || rejected.ValidationResult.Passed {
		t.Fatalf("expected validation_result.passed=false, got %+v", rejected.ValidationResult)
	}
	if len(rejected.ValidationResult.Missing) != 1 || rejected.ValidationResult.Missing[0] != "employee_id" {
		t.Fatalf("expected missing=[employee_id], got %v", rejected.ValidationResult.Missing)
	}
	if rejected.RejectionReason == nil || *rejected.RejectionReason == "" {
		t.Fatal("expected a non-empty rejection_reason")
	}

	complete, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-2", CorrelationID: "draft-complete",
		SubmittedValues: map[string]string{"amount": "50", "employee_id": "E-100"},
	})
	if err != nil {
		t.Fatalf("save complete draft: %v", err)
	}
	if _, err := s.SubmitForm(ctx, domain.SubmitFormParams{SubmissionID: complete.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "submitter-2", CorrelationID: "submit-complete"}); err != nil {
		t.Fatalf("submit complete: %v", err)
	}
	accepted, err := s.ValidateSubmission(ctx, domain.ValidateSubmissionParams{SubmissionID: complete.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "validator-1", CorrelationID: "validate-complete"})
	if err != nil || accepted.Status != domain.FormSubmissionAccepted {
		t.Fatalf("expected ACCEPTED, status=%v err=%v", accepted, err)
	}
	if accepted.ValidationResult == nil || !accepted.ValidationResult.Passed {
		t.Fatalf("expected validation_result.passed=true, got %+v", accepted.ValidationResult)
	}
}

// TestPgStore_ValidateSubmission_RejectsNonSubmitted proves
// ValidateSubmission refuses to run against a DRAFT (never submitted)
// submission.
func TestPgStore_ValidateSubmission_RejectsNonSubmitted(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	form := newFormForTest(t, s, ctx, "form-validate-draft", []string{"amount"})

	draft, err := s.SaveDraft(ctx, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "draft-only",
		SubmittedValues: map[string]string{"amount": "50"},
	})
	if err != nil {
		t.Fatalf("save draft: %v", err)
	}
	if _, err := s.ValidateSubmission(ctx, domain.ValidateSubmissionParams{SubmissionID: draft.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "validator-1", CorrelationID: "validate-draft"}); err != domain.ErrFormSubmissionNotSubmitted {
		t.Fatalf("expected ErrFormSubmissionNotSubmitted, got %v", err)
	}
}

func TestPgStore_GetForm_UnknownForm_ReturnsNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	if _, err := s.GetForm(tenantCtx(testTenantID), testTenantID, "00000000-0000-0000-0000-000000000000"); err != domain.ErrFormNotFound {
		t.Fatalf("expected ErrFormNotFound, got %v", err)
	}
}

func TestPgStore_GetSubmission_UnknownSubmission_ReturnsNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	if _, err := s.GetSubmission(tenantCtx(testTenantID), testTenantID, "00000000-0000-0000-0000-000000000000"); err != domain.ErrFormSubmissionNotFound {
		t.Fatalf("expected ErrFormSubmissionNotFound, got %v", err)
	}
}
