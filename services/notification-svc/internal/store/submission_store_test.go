package store_test

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// Migration 000013. A stranded row WITH the marker was handed to a provider:
// it is found as Submitted, cannot be revived, and becomes PENDING_UNKNOWN
// with an UNKNOWN attempt recorded and outcome_unknown enqueued.
func TestSubmission_StrandedAfterSubmitBecomesUnknown(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-sub-stranded")

	submittedAt := time.Now().UTC().Add(-time.Hour)
	if err := s.BeginSubmission(ctx, n.NotificationID, "tenant-a", submittedAt); err != nil {
		t.Fatalf("BeginSubmission: %v", err)
	}
	staleBefore := time.Now().UTC().Add(-15 * time.Minute)

	found, err := s.FindStrandedDeliveries(ctx, staleBefore, 10)
	if err != nil {
		t.Fatalf("FindStrandedDeliveries: %v", err)
	}
	if len(found) != 1 || !found[0].Submitted {
		t.Fatalf("stranded = %+v, want one Submitted row", found)
	}
	if revived, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC()); err != nil || revived {
		t.Fatalf("a submitted row was revived (revived=%v err=%v) — the blind re-send §6.2 forbids", revived, err)
	}
	marked, err := s.MarkStrandedUnknown(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC())
	if err != nil || !marked {
		t.Fatalf("MarkStrandedUnknown: marked=%v err=%v", marked, err)
	}

	got, err := s.GetNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if got.Status != domain.StatusPendingUnknown || got.FailureReason == "" || got.SentAt == nil {
		t.Fatalf("after mark: status=%s reason=%q sent_at=%v", got.Status, got.FailureReason, got.SentAt)
	}
	attempts, _ := s.ListAttempts(ctx, n.NotificationID)
	if len(attempts) != 1 || attempts[0].Outcome != domain.AttemptOutcomeUnknown {
		t.Fatalf("attempts = %+v, want one UNKNOWN", attempts)
	}
	if types := eventTypesFor(t, pool, n.NotificationID); len(types) != 1 || types[0] != "notification.outcome_unknown" {
		t.Fatalf("events = %v, want [notification.outcome_unknown]", types)
	}
}

// Without the marker, a stranded row never reached a provider: it is not
// Submitted and is revived as before.
func TestSubmission_StrandedBeforeSubmitIsRevived(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-sub-never")
	backdate := `UPDATE notifications SET created_at = now() - interval '1 hour' WHERE notification_id = $1`
	asTenant(t, pool, "tenant-a", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, backdate, n.NotificationID)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("backdate matched %d rows, want 1", tag.RowsAffected())
		}
		return err
	})
	staleBefore := time.Now().UTC().Add(-15 * time.Minute)
	found, _ := s.FindStrandedDeliveries(ctx, staleBefore, 10)
	if len(found) != 1 || found[0].Submitted {
		t.Fatalf("stranded = %+v, want one unsubmitted row", found)
	}
	if marked, _ := s.MarkStrandedUnknown(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC()); marked {
		t.Fatal("an unsubmitted row was marked unknown")
	}
	if revived, err := s.ReviveStranded(ctx, n.NotificationID, "tenant-a", staleBefore, time.Now().UTC()); err != nil || !revived {
		t.Fatalf("ReviveStranded: revived=%v err=%v", revived, err)
	}
}

// Recording the outcome clears the marker, so a concluded row is never taken
// for a lost submission.
func TestSubmission_ConclusionClearsTheMarker(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-sub-clear")
	if err := s.BeginSubmission(ctx, n.NotificationID, "tenant-a", time.Now().UTC()); err != nil {
		t.Fatalf("BeginSubmission: %v", err)
	}
	at := time.Now().UTC()
	if err := s.ScheduleRetry(ctx, n.NotificationID, "tenant-a", "421", at, at.Add(time.Minute), domain.AttemptMeta{}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	var marked bool
	read := `SELECT submitting_since IS NOT NULL FROM notifications WHERE notification_id = $1`
	asTenant(t, pool, "tenant-a", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, read, n.NotificationID).Scan(&marked)
	})
	if marked {
		t.Fatal("ScheduleRetry left the submission marker set")
	}
}

// asTenant runs fn in a transaction scoped to tenantID, so raw SQL in a test is
// subject to the same row-level security as the store. Without it, a suite run
// as the owning non-superuser role would have its UPDATE silently match
// nothing under FORCE ROW LEVEL SECURITY.
func asTenant(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := fn(tx); err != nil {
		t.Fatalf("as tenant %s: %v", tenantID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
