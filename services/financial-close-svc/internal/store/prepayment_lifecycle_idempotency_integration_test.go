//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11's prepayment
// lifecycle slice (ACC-08, the mirror of the ACC-07 accrual tests in
// accrual_lifecycle_idempotency_integration_test.go): Approve/Modify/
// Terminate now claim an Idempotency-Key the same way the four creates do.
package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func draftPrepayment(description string) *domain.PrepaymentSchedule {
	return &domain.PrepaymentSchedule{
		CurrencyCode: storeTestCurrency,
		ScheduleID:   uuid.New().String(), LegalEntityID: "le-1", Description: description,
		TotalAmount: 1200, StartFiscalPeriod: "2026-01", PeriodCount: 12,
		DebitAccountCode: "6200-Insurance", CreditAccountCode: "1400-PrepaidInsurance",
		Status: domain.PrepaymentStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_ApprovePrepayment_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftPrepayment("approve replay")
	if _, err := s.CreatePrepaymentSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreatePrepaymentSchedule: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.ApprovePrepaymentSchedule(ctx, sch, "approver-1", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first approve: replay=%v err=%v", replay, err)
	}
	if sch.Status != domain.PrepaymentStatusApproved {
		t.Fatalf("status after approve = %q", sch.Status)
	}

	retrySch := &domain.PrepaymentSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.ApprovePrepaymentSchedule(ctx, retrySch, "approver-1", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried approve: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.Status != domain.PrepaymentStatusApproved {
		t.Fatalf("replay status = %q, want APPROVED", retrySch.Status)
	}
}

func TestPgStore_ModifyPrepayment_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftPrepayment("modify replay")
	if _, err := s.CreatePrepaymentSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreatePrepaymentSchedule: %v", err)
	}
	if _, err := s.ApprovePrepaymentSchedule(ctx, sch, "approver-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("approve: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.ModifyFuturePrepaymentSchedule(ctx, sch, 2400, 24, key, testRequestHash); err != nil || replay {
		t.Fatalf("first modify: replay=%v err=%v", replay, err)
	}
	if sch.TotalAmount != 2400 || sch.PeriodCount != 24 {
		t.Fatalf("modify did not land: %+v", sch)
	}

	retrySch := &domain.PrepaymentSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.ModifyFuturePrepaymentSchedule(ctx, retrySch, 2400, 24, key, testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retried modify: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.TotalAmount != 2400 || retrySch.PeriodCount != 24 {
		t.Fatalf("replay did not return the modified shape: %+v", retrySch)
	}
}

func TestPgStore_TerminatePrepayment_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftPrepayment("terminate replay")
	if _, err := s.CreatePrepaymentSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreatePrepaymentSchedule: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.TerminatePrepaymentSchedule(ctx, sch, "terminator-1", "contract ended",
		domain.TerminationTreatmentWriteOff, key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first terminate: replay=%v err=%v", replay, err)
	}
	if sch.Status != domain.PrepaymentStatusTerminated {
		t.Fatalf("status after terminate = %q", sch.Status)
	}

	// Retry under the same key replays, even though the schedule is now
	// TERMINATED (which a fresh attempt must refuse).
	retrySch := &domain.PrepaymentSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.TerminatePrepaymentSchedule(ctx, retrySch, "terminator-1", "contract ended",
		domain.TerminationTreatmentWriteOff, key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried terminate: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.Status != domain.PrepaymentStatusTerminated {
		t.Fatalf("replay status = %q, want TERMINATED", retrySch.Status)
	}

	// A genuinely new attempt (a different key) against the now-TERMINATED
	// schedule must still be refused.
	if _, err := s.TerminatePrepaymentSchedule(ctx, sch, "terminator-1", "retry with new key",
		domain.TerminationTreatmentWriteOff, uuid.NewString(), testRequestHash, now); !errors.Is(err, domain.ErrInvalidPrepaymentTransition) {
		t.Fatalf("new terminate attempt on an already-terminated schedule: want ErrInvalidPrepaymentTransition, got %v", err)
	}
}
