//go:build integration

// Real-Postgres regression test for SPEC_DEVIATIONS A11's ACC-10 slice:
// ApproveFXRevaluationRun now claims an Idempotency-Key (the mirror of the
// ACC-07/08/09 tests in the sibling *_idempotency_integration_test.go
// files). PostRevaluation is not part of this claim — it already answers a
// replay with the run's current state via its own already-POSTED check, a
// handler-level guard rather than command_idempotency.
package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_ApproveFXRevaluation_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	run := &domain.FXRevaluationRun{
		CurrencyCode: storeTestCurrency,
		RunID:        uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
		Status: domain.FXRevaluationStatusReview, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreateFXRevaluationRun(ctx, run, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateFXRevaluationRun: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.ApproveFXRevaluationRun(ctx, run, "approver-1", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first approve: replay=%v err=%v", replay, err)
	}
	if run.Status != domain.FXRevaluationStatusApproved {
		t.Fatalf("status after approve = %q", run.Status)
	}

	retryRun := &domain.FXRevaluationRun{RunID: run.RunID}
	replay, err := s.ApproveFXRevaluationRun(ctx, retryRun, "approver-1", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried approve: want replay, got replay=%v err=%v", replay, err)
	}
	if retryRun.Status != domain.FXRevaluationStatusApproved {
		t.Fatalf("replay status = %q, want APPROVED", retryRun.Status)
	}
}
