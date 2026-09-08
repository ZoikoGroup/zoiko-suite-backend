package store_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// Integration tests for the stranded-delivery pair, against real Postgres.
//
// A stranded notification is PENDING with next_attempt_at NULL — the state
// ClaimRetry creates deliberately and that means "in flight right now".
// FindDueRetries requires next_attempt_at IS NOT NULL, so nothing in the
// service ever touched such a row again: never delivered, never failed, never
// re-attempted. Five were measured on the dev database on 2026-09-08, six days
// old with zero delivery attempts.
//
// These run against the real predicates rather than a stub because the whole
// defect was a query that could not see a row, which is exactly the thing a
// map-backed fake cannot reproduce.

// A row in flight for longer than the threshold is found.
func TestPgStore_FindStranded_FindsAnAbandonedInFlightRow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-stranded-1")
	// Backdate creation so the row is older than any threshold used below.
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	// PENDING with no schedule and no attempt: exactly what a crash between
	// create and conclude leaves behind, and what the five live rows looked
	// like.
	staleBefore := time.Now().UTC().Add(-15 * time.Minute)

	// The defect, stated as an assertion: the due poll cannot see it.
	due, err := s.FindDueRetries(context.Background(), time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("find due: %v", err)
	}
	for _, d := range due {
		if d.NotificationID == n.NotificationID {
			t.Fatal("FindDueRetries returned an in-flight row; the premise of the sweep is that it cannot")
		}
	}

	stranded, err := s.FindStrandedDeliveries(context.Background(), staleBefore, 50)
	if err != nil {
		t.Fatalf("find stranded: %v", err)
	}
	if !containsID(stranded, n.NotificationID) {
		t.Fatalf("the stranded row was not found; it would never be delivered")
	}
}

// COALESCE(last_attempt_at, created_at) is the in-flight clock. A row stranded
// before its FIRST attempt has no last_attempt_at at all — the majority of the
// real case — and a predicate on last_attempt_at alone would skip exactly
// those.
func TestPgStore_FindStranded_UsesCreatedAtWhenNeverAttempted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-stranded-never")
	n.CreatedAt = time.Now().UTC().Add(-1 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	stranded, err := s.FindStrandedDeliveries(context.Background(),
		time.Now().UTC().Add(-15*time.Minute), 50)
	if err != nil {
		t.Fatalf("find stranded: %v", err)
	}
	if !containsID(stranded, n.NotificationID) {
		t.Error("a never-attempted stranded row was skipped — last_attempt_at is NULL for these")
	}
}

// The safety property. A notification that has been in flight only briefly is
// probably being attempted right now, and reviving it would send the message
// twice.
func TestPgStore_FindStranded_LeavesRecentInFlightRowsAlone(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-stranded-fresh")
	// Created just now: in flight, but nowhere near stale.
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	stranded, err := s.FindStrandedDeliveries(context.Background(),
		time.Now().UTC().Add(-15*time.Minute), 50)
	if err != nil {
		t.Fatalf("find stranded: %v", err)
	}
	if containsID(stranded, n.NotificationID) {
		t.Error("a freshly-created in-flight row was reported stranded — this would duplicate a live send")
	}
}

// A concluded notification is never stranded, whatever its age. SENT rows in
// particular must never be revived: that is the one case where a duplicate is
// certain rather than possible.
func TestPgStore_FindStranded_IgnoresConcludedRows(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	for _, tc := range []struct {
		corr   string
		status string
		reason string
	}{
		{"corr-stranded-sent", "SENT", ""},
		{"corr-stranded-failed", "FAILED", "mailbox rejected"},
	} {
		n := newNotification("tenant-a", "le-us", "principal-2", tc.corr)
		n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
		if _, err := s.CreateNotification(ctx, n); err != nil {
			t.Fatalf("create %s: %v", tc.corr, err)
		}
		concluded := time.Now().UTC().Add(-90 * time.Minute)
		if err := s.CompleteDelivery(ctx, n.NotificationID, tc.status, tc.reason, "", &concluded); err != nil {
			t.Fatalf("complete %s: %v", tc.corr, err)
		}

		stranded, err := s.FindStrandedDeliveries(context.Background(),
			time.Now().UTC().Add(-15*time.Minute), 50)
		if err != nil {
			t.Fatalf("find stranded: %v", err)
		}
		if containsID(stranded, n.NotificationID) {
			t.Errorf("%s row reported stranded — reviving a concluded delivery re-sends it", tc.status)
		}
	}
}

// A row with a schedule already on it belongs to the due path, not the sweep.
// Both reporting it would have two code paths racing for the same
// notification.
func TestPgStore_FindStranded_IgnoresRowsAlreadyScheduled(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-stranded-scheduled")
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	attemptedAt := time.Now().UTC().Add(-time.Hour)
	next := time.Now().UTC().Add(-30 * time.Minute) // due, and in the past
	if err := s.ScheduleRetry(ctx, n.NotificationID, "tenant-a", "smtp timeout", attemptedAt, next); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	stranded, err := s.FindStrandedDeliveries(context.Background(),
		time.Now().UTC().Add(-15*time.Minute), 50)
	if err != nil {
		t.Fatalf("find stranded: %v", err)
	}
	if containsID(stranded, n.NotificationID) {
		t.Error("a scheduled row was reported stranded; the due path already owns it")
	}
}

// ── ReviveStranded ──────────────────────────────────────────────────────────

// Reviving puts the row back where the due poll can see it — which is the
// whole outcome the feature exists for.
func TestPgStore_ReviveStranded_MakesTheRowVisibleToTheDuePoll(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-revive-1")
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	staleBefore := time.Now().UTC().Add(-15 * time.Minute)
	now := time.Now().UTC()

	revived, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a", staleBefore, now)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if !revived {
		t.Fatal("revive reported no row affected")
	}

	due, err := s.FindDueRetries(context.Background(), time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("find due: %v", err)
	}
	if !containsID(due, n.NotificationID) {
		t.Error("the revived notification is still invisible to the due poll")
	}
}

// The attempt count is preserved, so being stranded does not hand a
// notification a fresh retry budget and the policy ceiling still bounds it.
func TestPgStore_ReviveStranded_KeepsTheAttemptCount(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-revive-attempts")
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	// One failed attempt, then claimed and abandoned: attempts = 1, no schedule.
	attemptedAt := time.Now().UTC().Add(-time.Hour)
	if err := s.ScheduleRetry(ctx, n.NotificationID, "tenant-a", "smtp timeout",
		attemptedAt, time.Now().UTC().Add(-45*time.Minute)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := s.ClaimRetry(ctx, n.NotificationID, "tenant-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	before, err := s.GetNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	if _, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a",
		time.Now().UTC().Add(-15*time.Minute), time.Now().UTC()); err != nil {
		t.Fatalf("revive: %v", err)
	}

	after, err := s.GetNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.DeliveryAttempts != before.DeliveryAttempts {
		t.Errorf("attempts moved from %d to %d; a stranded row must not get a fresh budget",
			before.DeliveryAttempts, after.DeliveryAttempts)
	}
}

// The staleness predicate is repeated inside the UPDATE and is the claim: a
// row that is not stale does not get revived even if the caller names it.
// Without this a lost race would drag a live in-flight notification back onto
// the schedule and send it twice.
func TestPgStore_ReviveStranded_RefusesARowThatIsNotStale(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-revive-fresh")
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	revived, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a",
		time.Now().UTC().Add(-15*time.Minute), time.Now().UTC())
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived {
		t.Error("a fresh in-flight row was revived; this is the duplicate-send path")
	}
}

// Second caller gets false. The row is the claim, so two replicas sweeping the
// same second cannot both take it.
func TestPgStore_ReviveStranded_IsClaimedOnce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-revive-once")
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	staleBefore := time.Now().UTC().Add(-15 * time.Minute)

	first, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC())
	if err != nil || !first {
		t.Fatalf("first revive: revived=%v err=%v", first, err)
	}
	second, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC())
	if err != nil {
		t.Fatalf("second revive: %v", err)
	}
	if second {
		t.Error("the same stranded row was revived twice")
	}
}

// Another tenant cannot revive it. The sweep's poll is the one cross-tenant
// read in the service, and this is what stops it becoming a cross-tenant
// write.
func TestPgStore_ReviveStranded_DoesNotCrossTenants(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-revive-tenant")
	n.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	revived, err := s.ReviveStranded(tenantCtx("tenant-b"), n.NotificationID, "tenant-b",
		time.Now().UTC().Add(-15*time.Minute), time.Now().UTC())
	if err != nil {
		t.Fatalf("cross-tenant revive: %v", err)
	}
	if revived {
		t.Error("tenant-b revived tenant-a's notification")
	}

	// And it is untouched, so the refusal is not a partial write.
	still, err := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if still.NextAttemptAt != nil {
		t.Error("the cross-tenant revive scheduled it anyway")
	}
}

func containsID(rows []domain.DueRetry, id string) bool {
	for _, r := range rows {
		if r.NotificationID == id {
			return true
		}
	}
	return false
}
