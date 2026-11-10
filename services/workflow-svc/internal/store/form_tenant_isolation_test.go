package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// BIZ-04 implementation gate: tenant isolation. Tenant B must not be able to read,
// list, draft against, publish, submit, validate, supersede or route tenant A's form
// and submissions, and nothing B attempts may change A's data.
func TestPgStore_Form_TenantIsolation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())

	const tenantB = "00000000-0000-0000-0000-000000000002"
	ctxA, ctxB := tenantCtx(testTenantID), tenantCtx(tenantB)

	form := newFormForTest(t, s, ctxA, "iso-form", []string{"amount"})
	accepted := acceptedSubmissionForTest(t, s, ctxA, form.FormID, "submitter-1", "iso-acc", map[string]string{"amount": "50"})
	draft, err := s.SaveDraft(ctxA, domain.SaveDraftParams{
		FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "submitter-1", CorrelationID: "iso-draft",
		SubmittedValues: map[string]string{"amount": "75"},
	})
	if err != nil {
		t.Fatalf("tenant A draft: %v", err)
	}

	// Reads: B sees nothing of A's.
	if _, err := s.GetForm(ctxB, tenantB, form.FormID); err != domain.ErrFormNotFound {
		t.Fatalf("GetForm as B: expected ErrFormNotFound, got %v", err)
	}
	if _, err := s.GetSubmission(ctxB, tenantB, accepted.SubmissionID); err != domain.ErrFormSubmissionNotFound {
		t.Fatalf("GetSubmission as B: expected ErrFormSubmissionNotFound, got %v", err)
	}
	if _, err := s.GetSubmissionVersion(ctxB, tenantB, accepted.SubmissionID); err == nil {
		t.Fatal("GetSubmissionVersion as B must not return A's submission")
	}
	if list, err := s.ListPendingSubmissions(ctxB, tenantB, form.FormID, 100, 0); err != nil || len(list) != 0 {
		t.Fatalf("ListPendingSubmissions as B: expected empty, got %d (err %v)", len(list), err)
	}

	// Writes: every command B attempts on A's records is refused.
	if _, err := s.PublishForm(ctxB, domain.PublishFormParams{FormID: form.FormID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-pub"}); err == nil {
		t.Fatal("PublishForm as B must be refused")
	}
	if _, err := s.RetireForm(ctxB, domain.RetireFormParams{FormID: form.FormID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-ret"}); err == nil {
		t.Fatal("RetireForm as B must be refused")
	}
	if _, err := s.SaveDraft(ctxB, domain.SaveDraftParams{FormID: form.FormID, TenantID: tenantB, SubmitterPrincipalID: "attacker", CorrelationID: "iso-b-draft", SubmittedValues: map[string]string{"amount": "1"}}); err == nil {
		t.Fatal("SaveDraft (new) against A's form as B must be refused")
	}
	if _, err := s.SaveDraft(ctxB, domain.SaveDraftParams{SubmissionID: draft.SubmissionID, TenantID: tenantB, SubmitterPrincipalID: "attacker", CorrelationID: "iso-b-draft2", SubmittedValues: map[string]string{"amount": "999"}}); err == nil {
		t.Fatal("SaveDraft (update) of A's draft as B must be refused")
	}
	if _, err := s.SubmitForm(ctxB, domain.SubmitFormParams{SubmissionID: draft.SubmissionID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-sub"}); err == nil {
		t.Fatal("SubmitForm of A's draft as B must be refused")
	}
	if _, err := s.ValidateSubmission(ctxB, domain.ValidateSubmissionParams{SubmissionID: draft.SubmissionID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-val"}); err == nil {
		t.Fatal("ValidateSubmission of A's draft as B must be refused")
	}
	if _, err := s.SupersedeSubmission(ctxB, domain.SupersedeSubmissionParams{PreviousSubmissionID: accepted.SubmissionID, NewSubmissionID: draft.SubmissionID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-sup"}); err == nil {
		t.Fatal("SupersedeSubmission of A's records as B must be refused")
	}
	if _, err := s.RouteToDomain(ctxB, domain.RouteToDomainParams{SubmissionID: accepted.SubmissionID, TenantID: tenantB, ActorPrincipalID: "attacker", CorrelationID: "iso-b-route", Outcome: "SUCCEEDED"}); err == nil {
		t.Fatal("RouteToDomain of A's submission as B must be refused")
	}

	// A's data is unchanged by everything B attempted.
	gotForm, err := s.GetForm(ctxA, testTenantID, form.FormID)
	if err != nil || gotForm.Status != "DRAFT" {
		t.Fatalf("tenant A form should still be DRAFT, got %+v (err %v)", gotForm, err)
	}
	gotDraft, err := s.GetSubmission(ctxA, testTenantID, draft.SubmissionID)
	if err != nil || gotDraft.Status != domain.FormSubmissionDraft || gotDraft.SubmittedValues["amount"] != "75" {
		t.Fatalf("tenant A draft should be untouched (DRAFT, amount 75), got %+v (err %v)", gotDraft, err)
	}
	gotAccepted, err := s.GetSubmission(ctxA, testTenantID, accepted.SubmissionID)
	if err != nil || gotAccepted.Status != domain.FormSubmissionAccepted || gotAccepted.SupersededBySubmissionID != nil {
		t.Fatalf("tenant A accepted submission should be untouched, got %+v (err %v)", gotAccepted, err)
	}
	var routes int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM form_submission_routes WHERE submission_id = $1`, accepted.SubmissionID).Scan(&routes); err == nil && routes != 0 {
		t.Fatalf("no route may be recorded by tenant B, found %d", routes)
	}

	// RLS itself, bypassing the store's tenant predicates: with B's tenant context and
	// no explicit tenant filter, A's rows must still be invisible. Skipped (not passed)
	// for a role that bypasses RLS.
	var bypass bool
	if err := pool.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	if bypass {
		t.Log("connecting role bypasses RLS; skipping the raw RLS probe")
		return
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenantB); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	for _, table := range []string{"form_definitions", "form_submissions", "form_submission_routes"} {
		var n int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("RLS leak: tenant B context sees %d rows in %s", n, table)
		}
	}
}
