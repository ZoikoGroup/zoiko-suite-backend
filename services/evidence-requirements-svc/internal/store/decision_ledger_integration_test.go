package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/store"
)

// DL-6: a new evaluation queues its governance-ledger record in the same
// transaction; a replayed evaluation (same Idempotency-Key) queues nothing new;
// with decision capture off nothing is queued.
func TestPgStore_DL6_EvaluationQueuesOneLedgerRecord(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	s.SetDecisionLedger(true)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	reqID := uuid.New().String()
	newEval := func(key string) *domain.EvidenceEvaluation {
		return &domain.EvidenceEvaluation{
			EvaluationID: uuid.New().String(), TenantID: tenantID, LegalEntityID: uuid.New().String(),
			DomainCode: "FINANCE", ActionType: "JOURNAL_POST", Outcome: domain.OutcomeMissing,
			UnmetPayload:            json.RawMessage(`[{"evidence_requirement_id":"` + reqID + `","evidence_type":"DOCUMENT","reason":"missing"}]`),
			PresentArtifactsPayload: json.RawMessage(`[]`), RequirementIDs: []string{reqID},
			EvaluatedForPrincipalID: "p-1", CorrelationID: "c-dl6", IdempotencyKey: key,
		}
	}
	count := func(decisionID string) int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM decision_log_outbox WHERE decision_id = $1`, decisionID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	key := "k-" + uuid.New().String()
	e := newEval(key)
	if created, err := s.RecordEvaluation(ctx, e, nil); err != nil || !created {
		t.Fatalf("record: created=%v err=%v", created, err)
	}
	if n := count(e.EvaluationID); n != 1 {
		t.Fatalf("ledger records for the evaluation: %d, want 1", n)
	}
	var outcome, action, basis string
	if err := pool.QueryRow(context.Background(), `SELECT record->>'outcome', record->>'action_type', record->>'rule_basis'
		FROM decision_log_outbox WHERE decision_id = $1`, e.EvaluationID).Scan(&outcome, &action, &basis); err != nil {
		t.Fatal(err)
	}
	if outcome != "MISSING" || action != "EVIDENCE_JOURNAL_POST" || len(basis) == 0 {
		t.Fatalf("record: %s %s %s", outcome, action, basis)
	}

	replay := newEval(key)
	replayID := replay.EvaluationID // RecordEvaluation answers a replay with the stored evaluation
	if created, err := s.RecordEvaluation(ctx, replay, nil); err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if n := count(replayID); n != 0 {
		t.Fatalf("a replay queued %d new records", n)
	}
	if n := count(e.EvaluationID); n != 1 {
		t.Fatalf("the original evaluation has %d records after the replay, want 1", n)
	}

	s.SetDecisionLedger(false)
	off := newEval("k-" + uuid.New().String())
	if _, err := s.RecordEvaluation(ctx, off, nil); err != nil {
		t.Fatal(err)
	}
	if n := count(off.EvaluationID); n != 0 {
		t.Fatalf("decision capture off queued %d records", n)
	}
}
