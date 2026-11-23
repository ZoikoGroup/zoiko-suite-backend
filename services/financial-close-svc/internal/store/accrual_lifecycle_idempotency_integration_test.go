//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11's accrual lifecycle
// slice: Submit/Approve/Cancel/Amend now claim an Idempotency-Key the same
// way the four creates already did. Before this pass, a lost-response retry
// of any of these got 422 invalid_transition instead of the schedule it had
// already moved.
package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_SubmitAccrual_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("submit replay")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}

	key := uuid.NewString()
	hash := testRequestHash
	if replay, err := s.SubmitAccrualSchedule(ctx, sch, "submitter-1", key, hash, now); err != nil || replay {
		t.Fatalf("first submit: replay=%v err=%v", replay, err)
	}
	if sch.Status != domain.AccrualStatusPendingApproval {
		t.Fatalf("status after submit = %q", sch.Status)
	}

	// A retry under the same key must replay, not 422 on a schedule that is
	// no longer DRAFT.
	retrySch := &domain.AccrualSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.SubmitAccrualSchedule(ctx, retrySch, "submitter-1", key, hash, now)
	if err != nil || !replay {
		t.Fatalf("retried submit: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.Status != domain.AccrualStatusPendingApproval {
		t.Fatalf("replay status = %q, want PENDING_APPROVAL", retrySch.Status)
	}
}

// TestPgStore_SubmitAccrual_SameKeyDifferentSchedule_IsConflict proves the
// request-hash fold-in: Submit has no body, so without the schedule id
// inside the hash, reusing a key against a second schedule would silently
// replay the first schedule's data under the second schedule's id.
func TestPgStore_SubmitAccrual_SameKeyDifferentSchedule_IsConflict(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	first := draftAccrual("schedule A")
	if _, err := s.CreateAccrualSchedule(ctx, first, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := draftAccrual("schedule B")
	if _, err := s.CreateAccrualSchedule(ctx, second, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("create second: %v", err)
	}

	key := uuid.NewString()
	// The handler folds the schedule id into the hash; simulate that here
	// directly so this test does not depend on the handler package.
	if _, err := s.SubmitAccrualSchedule(ctx, first, "submitter-1", key, "hash-for-"+first.ScheduleID, now); err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if _, err := s.SubmitAccrualSchedule(ctx, second, "submitter-1", key, "hash-for-"+second.ScheduleID, now); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key against a different schedule: want ErrIdempotencyKeyConflict, got %v", err)
	}
	// second must be untouched: still DRAFT.
	got, err := s.GetAccrualSchedule(ctx, second.ScheduleID)
	if err != nil || got.Status != domain.AccrualStatusDraft {
		t.Fatalf("second schedule was mutated by the conflicting call: %+v (err %v)", got, err)
	}
}

func TestPgStore_AmendAccrual_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("amend replay")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	if _, err := s.SubmitAccrualSchedule(ctx, sch, "submitter-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ApproveAccrualSchedule(ctx, sch, "approver-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("approve: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.AmendAccrualSchedule(ctx, sch, 2400, 12, key, testRequestHash); err != nil || replay {
		t.Fatalf("first amend: replay=%v err=%v", replay, err)
	}
	if sch.TotalAmount != 2400 || sch.PeriodCount != 12 {
		t.Fatalf("amend did not land: %+v", sch)
	}

	retrySch := &domain.AccrualSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.AmendAccrualSchedule(ctx, retrySch, 2400, 12, key, testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retried amend: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.TotalAmount != 2400 || retrySch.PeriodCount != 12 {
		t.Fatalf("replay did not return the amended shape: %+v", retrySch)
	}
}

func TestPgStore_CancelAccrual_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("cancel replay")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.CancelAccrualSchedule(ctx, sch, "canceller-1", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first cancel: replay=%v err=%v", replay, err)
	}
	if sch.Status != domain.AccrualStatusCancelled {
		t.Fatalf("status after cancel = %q", sch.Status)
	}

	// Retry under the same key replays, even though the schedule is now
	// CANCELLED (which a fresh attempt must refuse — see the "new key"
	// case in TestPgStore_TransitionsWithSetClause_ApplyAgainstRealDB).
	retrySch := &domain.AccrualSchedule{ScheduleID: sch.ScheduleID}
	replay, err := s.CancelAccrualSchedule(ctx, retrySch, "canceller-1", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried cancel: want replay, got replay=%v err=%v", replay, err)
	}
	if retrySch.Status != domain.AccrualStatusCancelled {
		t.Fatalf("replay status = %q, want CANCELLED", retrySch.Status)
	}
}
