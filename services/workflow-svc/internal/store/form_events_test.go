package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// BIZ-04 events are written to the outbox in the same transaction as the change they
// announce. They used to be published by the handler after the commit with a failure only
// logged, so a broker problem lost them silently.
func TestPgStore_FormEvents_WrittenWithEachChange(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	types := func(aggregateID string) []string {
		rows, err := pool.Query(context.Background(), `SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, aggregateID)
		if err != nil {
			t.Fatalf("read outbox: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var ty string
			if err := rows.Scan(&ty); err != nil {
				t.Fatal(err)
			}
			out = append(out, ty)
		}
		return out
	}
	equal := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("events = %v, want %v", got, want)
			}
		}
	}

	form := newFormForTest(t, s, ctx, "ev-form", []string{"amount"})
	equal(types(form.FormID)) // creating a form announces nothing: the spec names only FormPublished
	if _, err := s.PublishForm(ctx, domain.PublishFormParams{FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-1", CorrelationID: "ev-publish"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	equal(types(form.FormID), "form.published")

	// A refused publish writes no event.
	if _, err := s.PublishForm(ctx, domain.PublishFormParams{FormID: form.FormID, TenantID: testTenantID, ActorPrincipalID: "approver-2", CorrelationID: "ev-publish-2"}); err == nil {
		t.Fatal("republish must be refused")
	}
	equal(types(form.FormID), "form.published")

	// accepted submission: received, validated, routed
	good, err := s.SaveDraft(ctx, domain.SaveDraftParams{FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "sub-1", CorrelationID: "ev-d1", SubmittedValues: map[string]string{"amount": "10"}})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	equal(types(good.SubmissionID)) // a draft announces nothing
	if _, err := s.SubmitForm(ctx, domain.SubmitFormParams{SubmissionID: good.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-1", CorrelationID: "ev-s1"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ValidateSubmission(ctx, domain.ValidateSubmissionParams{SubmissionID: good.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-1", CorrelationID: "ev-v1"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := s.RouteToDomain(ctx, domain.RouteToDomainParams{SubmissionID: good.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-1", CorrelationID: "ev-r1", Outcome: "SUCCEEDED", CommandReference: "cmd-1"}); err != nil {
		t.Fatalf("route: %v", err)
	}
	equal(types(good.SubmissionID), "form.submission.received", "form.submission.validated", "form.submission.routed")

	// rejected submission: received, rejected; a rejected one cannot be routed and writes no routed event
	bad, err := s.SaveDraft(ctx, domain.SaveDraftParams{FormID: form.FormID, TenantID: testTenantID, SubmitterPrincipalID: "sub-2", CorrelationID: "ev-d2", SubmittedValues: map[string]string{}})
	if err != nil {
		t.Fatalf("draft 2: %v", err)
	}
	if _, err := s.SubmitForm(ctx, domain.SubmitFormParams{SubmissionID: bad.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-2", CorrelationID: "ev-s2"}); err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	if _, err := s.ValidateSubmission(ctx, domain.ValidateSubmissionParams{SubmissionID: bad.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-2", CorrelationID: "ev-v2"}); err != nil {
		t.Fatalf("validate 2: %v", err)
	}
	if _, err := s.RouteToDomain(ctx, domain.RouteToDomainParams{SubmissionID: bad.SubmissionID, TenantID: testTenantID, ActorPrincipalID: "sub-2", CorrelationID: "ev-r2", Outcome: "SUCCEEDED"}); err == nil {
		t.Fatal("routing a rejected submission must be refused")
	}
	equal(types(bad.SubmissionID), "form.submission.received", "form.submission.rejected")

	// The event carries the correlation id and actor.
	var corr, actor string
	if err := pool.QueryRow(context.Background(), `SELECT correlation_id, actor_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='form.submission.routed'`, good.SubmissionID).Scan(&corr, &actor); err != nil {
		t.Fatalf("read routed event: %v", err)
	}
	if corr != "ev-r1" || actor != "sub-1" {
		t.Fatalf("routed event correlation=%q actor=%q, want ev-r1 / sub-1", corr, actor)
	}
}
