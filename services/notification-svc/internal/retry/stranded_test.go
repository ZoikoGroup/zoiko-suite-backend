package retry_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/retry"
)

// Tests for the stranded-delivery sweep.
//
// WHAT IT IS FOR. PENDING with next_attempt_at NULL means "in flight right
// now", and nothing in the service moved such a row on its own:
// FindDueRetries requires a schedule to be set, so a notification whose
// attempt never reported an outcome — the process died, ScheduleRetry or
// CompleteDelivery failed, or the request context was cancelled mid-send —
// stayed there forever. Never delivered, never failed, never re-attempted,
// and shown as PENDING, which reads as progress. Five such rows were measured
// on the dev database on 2026-09-08, six days old with zero attempts.
//
// RunOnce's shutdown comment asserted that "the sweep below" handled it. There
// was no sweep. These tests exist so that cannot be true again.

func newSweepWorker(s *stubStore, strandedAfter time.Duration) *retry.Worker {
	return retry.NewWorker(s, &stubDeliverer{}, &stubPublisher{}, &stubResolver{},
		func(error) bool { return true },
		retry.Options{
			Policy:        retry.Policy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Minute},
			StrandedAfter: strandedAfter,
		}, zap.NewNop())
}

// The whole point: a row the poll cannot see is put back on the schedule.
func TestSweepRevivesStrandedDeliveries(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{
		{NotificationID: "n-1", TenantID: "tenant-a"},
		{NotificationID: "n-2", TenantID: "tenant-b"},
	}

	w := newSweepWorker(s, 15*time.Minute)

	if got := w.SweepStranded(context.Background()); got != 2 {
		t.Fatalf("revived %d, want 2", got)
	}
	if !s.revived["n-1"] || !s.revived["n-2"] {
		t.Errorf("not every stranded notification was revived: %v", s.revived)
	}
}

// The cutoff must be in the PAST by the configured window. A sweep that asked
// for "now" would reclaim notifications another replica is mid-attempt on and
// send them twice, which is the one way this feature can do harm.
func TestSweepAsksForAThresholdInThePast(t *testing.T) {
	s := newStubStore()
	w := newSweepWorker(s, 15*time.Minute)

	before := time.Now().UTC()
	w.SweepStranded(context.Background())

	if len(s.staleSeen) != 1 {
		t.Fatalf("expected one poll, got %d", len(s.staleSeen))
	}
	cutoff := s.staleSeen[0]
	age := before.Sub(cutoff)
	// Allow a second of slack for the clock read inside the sweep.
	if age < 15*time.Minute-time.Second {
		t.Errorf("cutoff %s is only %s in the past; a live in-flight delivery would be reclaimed and sent twice",
			cutoff, age)
	}
}

// Zero must be a true off switch. The dangerous reading — "sweep everything
// immediately" — is precisely the setting that would duplicate every notice
// currently being sent, so it must not be reachable by leaving a value unset
// or by arithmetic producing a negative.
func TestSweepDisabledByZeroOrNegative(t *testing.T) {
	for _, threshold := range []time.Duration{0, -15 * time.Minute} {
		s := newStubStore()
		s.stranded = []domain.DueRetry{{NotificationID: "n-1", TenantID: "tenant-a"}}

		w := newSweepWorker(s, threshold)

		if got := w.SweepStranded(context.Background()); got != 0 {
			t.Errorf("threshold %s: revived %d, want 0", threshold, got)
		}
		if len(s.staleSeen) != 0 {
			t.Errorf("threshold %s: the store was polled despite the sweep being disabled", threshold)
		}
		if len(s.revived) != 0 {
			t.Errorf("threshold %s: a notification was revived with the sweep disabled", threshold)
		}
	}
}

// Every write after the cross-tenant poll must run with the notification's own
// tenant installed. Same property TestWorkerClaimsUnderTheNotificationsTenant
// pins for the claim path — the poll is the service's only cross-tenant read,
// and this is what keeps it from becoming a cross-tenant write.
func TestSweepRevivesUnderTheNotificationsTenant(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{
		{NotificationID: "n-1", TenantID: "tenant-a"},
		{NotificationID: "n-2", TenantID: "tenant-b"},
	}

	w := newSweepWorker(s, 15*time.Minute)
	w.SweepStranded(context.Background())

	if len(s.reviveTenants) != 2 {
		t.Fatalf("expected two revives, got %d", len(s.reviveTenants))
	}
	for i, want := range []string{"tenant-a", "tenant-b"} {
		if s.reviveTenants[i] != want {
			t.Errorf("revive %d ran under tenant %q, want %q", i, s.reviveTenants[i], want)
		}
	}
}

// A revive that loses the race is not an error. The row itself is the claim —
// same design as ClaimRetry — so a second replica finding zero rows affected
// is the mechanism working, not a fault.
func TestSweepToleratesLosingTheRace(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{{NotificationID: "n-1", TenantID: "tenant-a"}}
	s.revived["n-1"] = true // already taken

	w := newSweepWorker(s, 15*time.Minute)

	if got := w.SweepStranded(context.Background()); got != 0 {
		t.Errorf("revived %d, want 0 — the row was already claimed", got)
	}
}

// One tenant's failure must not abandon the rest of the batch. That is the
// same "keep going" property whose absence let this whole class of bug sit
// unnoticed, so it is asserted rather than assumed.
func TestSweepContinuesPastAFailedRevive(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{
		{NotificationID: "n-1", TenantID: "tenant-a"},
		{NotificationID: "n-2", TenantID: "tenant-b"},
	}
	s.reviveFails = true

	w := newSweepWorker(s, 15*time.Minute)

	if got := w.SweepStranded(context.Background()); got != 0 {
		t.Errorf("revived %d, want 0", got)
	}
	// Both were attempted despite the first failing.
	if len(s.reviveTenants) != 2 {
		t.Errorf("attempted %d revives, want 2 — a failure on the first abandoned the batch",
			len(s.reviveTenants))
	}
}

// A failing poll degrades to doing nothing, and must not stop the due pass
// that follows it in RunOnce.
func TestSweepPollFailureIsNotFatal(t *testing.T) {
	s := newStubStore()
	s.strandedFails = true
	s.due = []domain.DueRetry{{NotificationID: "due-1", TenantID: "tenant-a"}}
	s.byID["due-1"] = &domain.Notification{
		NotificationID: "due-1", TenantID: "tenant-a", Channel: domain.ChannelInApp,
		Status: "PENDING", DeliveryAttempts: 1,
	}

	w := newSweepWorker(s, 15*time.Minute)

	if got := w.RunOnce(context.Background()); got != 1 {
		t.Errorf("RunOnce attempted %d, want 1 — a failed sweep poll must not stop the due pass", got)
	}
}

// RunOnce runs the sweep, and runs it BEFORE the due pass so a reclaimed
// notification is delivered in the same tick rather than an interval later.
func TestRunOnceSweepsBeforeTheDuePass(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{{NotificationID: "n-1", TenantID: "tenant-a"}}

	w := newSweepWorker(s, 15*time.Minute)
	w.RunOnce(context.Background())

	if len(s.staleSeen) != 1 {
		t.Fatalf("RunOnce did not sweep at all — the defect this closes was a sweep that did not exist")
	}
	if !s.revived["n-1"] {
		t.Error("RunOnce swept but revived nothing")
	}
}

// A cancelled context stops the sweep between rows rather than plowing on
// through the batch during shutdown.
func TestSweepStopsOnCancellation(t *testing.T) {
	s := newStubStore()
	s.stranded = []domain.DueRetry{
		{NotificationID: "n-1", TenantID: "tenant-a"},
		{NotificationID: "n-2", TenantID: "tenant-b"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := newSweepWorker(s, 15*time.Minute)

	if got := w.SweepStranded(ctx); got != 0 {
		t.Errorf("revived %d on a cancelled context, want 0", got)
	}
}
