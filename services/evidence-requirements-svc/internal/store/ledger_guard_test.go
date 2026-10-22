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

// The evaluation ledger is evidence: once a determination is recorded it can
// be neither rewritten, deleted, nor emptied — even by the table owner.
func TestPgStore_EvaluationLedgerIsAppendOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	e := &domain.EvidenceEvaluation{
		EvaluationID: uuid.New().String(), TenantID: tenantID, LegalEntityID: uuid.New().String(),
		DomainCode: "FINANCE", ActionType: "JOURNAL_POST", Outcome: domain.OutcomeSatisfied,
		UnmetPayload: json.RawMessage(`[]`), PresentArtifactsPayload: json.RawMessage(`[]`),
		EvaluatedForPrincipalID: "p-1", CorrelationID: "c-1", IdempotencyKey: "k-" + uuid.New().String(),
	}
	if _, err := s.RecordEvaluation(ctx, e, nil); err != nil {
		t.Fatalf("RecordEvaluation: %v", err)
	}
	for name, stmt := range map[string]string{
		"UPDATE":   `UPDATE evidence_evaluations SET outcome = 'MISSING'`,
		"DELETE":   `DELETE FROM evidence_evaluations`,
		"TRUNCATE": `TRUNCATE evidence_evaluations`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err == nil {
			t.Errorf("%s of the evaluation ledger succeeded; it must be refused", name)
		}
	}
	got, err := s.GetEvaluation(ctx, e.EvaluationID)
	if err != nil || got == nil || got.Outcome != domain.OutcomeSatisfied {
		t.Fatalf("the recorded determination changed or vanished: %+v err=%v", got, err)
	}
}
