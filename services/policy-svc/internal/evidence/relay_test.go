package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/decisionlog"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/store"
)

type fakeStore struct {
	pending   []store.PendingEvidence
	evals     map[string]*domain.Evaluation
	delivered []string
	retried   []string
	failed    []string
	claims    int
}

func (f *fakeStore) ClaimEvidence(context.Context, int, time.Duration) ([]store.PendingEvidence, error) {
	f.claims++
	return f.pending, nil
}
func (f *fakeStore) FindEvaluationByID(_ context.Context, tenant, id string) (*domain.Evaluation, error) {
	ev, ok := f.evals[id]
	if !ok || ev.TenantID != tenant {
		return nil, domain.ErrEvaluationNotFound
	}
	return ev, nil
}
func (f *fakeStore) MarkEvidenceDelivered(_ context.Context, id string) error {
	f.delivered = append(f.delivered, id)
	return nil
}
func (f *fakeStore) MarkEvidenceRetry(_ context.Context, id, _ string, _ time.Time) error {
	f.retried = append(f.retried, id)
	return nil
}
func (f *fakeStore) MarkEvidenceFailed(_ context.Context, id, _ string) error {
	f.failed = append(f.failed, id)
	return nil
}

type fakeClient struct {
	errs map[string]error
	recs []decisionlog.Record
}

func (c *fakeClient) RecordDecision(_ context.Context, rec decisionlog.Record) error {
	c.recs = append(c.recs, rec)
	return c.errs[rec.DecisionID]
}

const tenant = "11111111-1111-1111-1111-111111111111"

func eval(id, decision string, entity *string) *domain.Evaluation {
	return &domain.Evaluation{
		EvaluationID: id, TenantID: tenant, DecisionID: decision, LegalEntityID: entity,
		PolicyType: "APPROVAL_THRESHOLD", RequestedByPrincipalID: "user-7", Result: "WITHIN_THRESHOLD",
		Outcome: "GRANTED", RuleBasis: "AP:v1", PolicyVersionID: "v1", CorrelationID: "corr",
		ActionContext: json.RawMessage(`{"amount":1}`), EvaluatedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
	}
}

func TestRelay_DeliversRetriesAndParks(t *testing.T) {
	entity := "33333333-3333-3333-3333-333333333333"
	st := &fakeStore{
		pending: []store.PendingEvidence{
			{EvaluationID: "e1", TenantID: tenant, Attempts: 1},
			{EvaluationID: "e2", TenantID: tenant, Attempts: 1},
			{EvaluationID: "e3", TenantID: tenant, Attempts: 1},
		},
		evals: map[string]*domain.Evaluation{
			"e1": eval("e1", "d1", nil),
			"e2": eval("e2", "d2", &entity),
			"e3": eval("e3", "d3", nil),
		},
	}
	cl := &fakeClient{errs: map[string]error{
		"d2": errors.New("governance-decision-log-svc returned 503"),
		"d3": decisionlog.ErrPermanent,
	}}
	r := NewRelay(st, cl, "svc-policy", time.Second, zap.NewNop())
	if n := r.RunOnce(context.Background()); n != 1 {
		t.Fatalf("expected 1 delivered, got %d", n)
	}
	if len(st.delivered) != 1 || st.delivered[0] != "e1" || len(st.retried) != 1 || st.retried[0] != "e2" ||
		len(st.failed) != 1 || st.failed[0] != "e3" {
		t.Fatalf("delivered %v, retried %v, failed %v", st.delivered, st.retried, st.failed)
	}

	// D-02: written as the service principal, the user kept in the context.
	rec := cl.recs[0]
	if rec.PrincipalID != "svc-policy" || rec.TenantID != tenant || rec.LegalEntityID != decisionlog.PlatformScope {
		t.Fatalf("unexpected envelope: %+v", rec)
	}
	if !bytes.Contains(rec.EvaluationContext, []byte(`"requested_by_principal_id":"user-7"`)) {
		t.Fatalf("the requester must travel in evaluation_context: %s", rec.EvaluationContext)
	}
	if cl.recs[1].LegalEntityID != entity {
		t.Fatalf("an entity-scoped evaluation is filed under its entity, got %s", cl.recs[1].LegalEntityID)
	}
}

// Redelivery must send byte-identical JSON, or the ledger's idempotency hash
// would refuse the retry of a record whose response was lost.
func TestRecordFor_IsDeterministic(t *testing.T) {
	a, _ := RecordFor(eval("e1", "d1", nil), "svc")
	b, _ := RecordFor(eval("e1", "d1", nil), "svc")
	if !bytes.Equal(a.EvaluationContext, b.EvaluationContext) || a.IdempotencyKey != b.IdempotencyKey || a.IdempotencyKey != "policy-svc:evaluation:e1" {
		t.Fatal("ledger records must be deterministic per evaluation")
	}
}

func TestRelay_IdleWithoutPrincipal(t *testing.T) {
	st := &fakeStore{pending: []store.PendingEvidence{{EvaluationID: "e1", TenantID: tenant}}}
	r := NewRelay(st, &fakeClient{}, "", time.Second, zap.NewNop())
	if r.RunOnce(context.Background()) != 0 || st.claims != 0 {
		t.Fatal("without a writer identity nothing is claimed; rows stay pending")
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if backoff(1) != 5*time.Second || backoff(2) != 10*time.Second || backoff(50) != maxBackoff {
		t.Fatalf("unexpected backoff: %v %v %v", backoff(1), backoff(2), backoff(50))
	}
}
