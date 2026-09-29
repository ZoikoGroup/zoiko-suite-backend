package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// Migration 000014 + 000011: a resend is one more governed attempt on the SAME
// notification, and the attempt chain keeps the original and the resend with
// its reason (§3.4).
func TestResend_PreservesTheChainOnTheSameCommunication(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-resend-chain")

	first := time.Now().UTC().Add(-time.Hour)
	if err := s.CompleteDelivery(ctx, n.NotificationID, "FAILED", "550 mailbox full", "", &first, "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("first conclusion: %v", err)
	}
	reopened, err := s.BeginResend(ctx, n.NotificationID, "tenant-a", "agent-7", "mailbox cleared", time.Now().UTC())
	if err != nil {
		t.Fatalf("BeginResend: %v", err)
	}
	if reopened.Status != domain.StatusPending || reopened.ResendCount != 1 || reopened.LastResendReason != "mailbox cleared" {
		t.Fatalf("reopened = status %s count %d reason %q", reopened.Status, reopened.ResendCount, reopened.LastResendReason)
	}
	second := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 queued", &second, "c",
		domain.AttemptMeta{Origin: domain.AttemptOriginResend, ResendReason: "mailbox cleared", ActorPrincipalID: "agent-7"}); err != nil {
		t.Fatalf("resend conclusion: %v", err)
	}

	attempts, err := s.ListAttempts(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (original + resend)", len(attempts))
	}
	if attempts[0].Outcome != domain.AttemptOutcomeFailed || attempts[0].FailureReason != "550 mailbox full" {
		t.Errorf("the original attempt's evidence changed: %+v", attempts[0])
	}
	if a := attempts[1]; a.Origin != domain.AttemptOriginResend || a.ResendReason != "mailbox cleared" ||
		a.ActorPrincipalID != "agent-7" || a.Outcome != domain.AttemptOutcomeAccepted || a.AttemptNumber != 2 {
		t.Errorf("resend attempt = %+v", a)
	}
}

// The schema refuses a resend attempt with no reason, whatever the caller does.
func TestResend_AttemptWithoutReasonIsRefusedByTheSchema(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-resend-noreason")
	at := time.Now().UTC()
	err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250", &at, "c",
		domain.AttemptMeta{Origin: domain.AttemptOriginResend})
	if err == nil {
		t.Fatal("a resend attempt without a reason was recorded")
	}
	got, _ := s.GetNotification(ctx, n.NotificationID)
	if got.Status != domain.StatusPending {
		t.Fatalf("the refused attempt half-applied: status %s", got.Status)
	}
}

// PENDING_UNKNOWN is not resendable: it must be resolved first (§6.2).
func TestResend_UnknownIsNotResendable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-resend-unknown")
	if err := s.MarkOutcomeUnknown(ctx, n.NotificationID, "tenant-a", "reset after DATA", time.Now().UTC(), "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("MarkOutcomeUnknown: %v", err)
	}
	if _, err := s.BeginResend(ctx, n.NotificationID, "tenant-a", "agent", "why", time.Now().UTC()); !errors.Is(err, domain.ErrNotResendable) {
		t.Fatalf("want ErrNotResendable, got %v", err)
	}
}

// Two concurrent resends cannot both reopen one notification.
func TestResend_ConcurrentResendsReopenOnce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-resend-race")
	at := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250", &at, "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	const racers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	reopened := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.BeginResend(ctx, n.NotificationID, "tenant-a", "agent", "customer asked", time.Now().UTC())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				reopened++
			} else if !errors.Is(err, domain.ErrNotResendable) {
				t.Errorf("racer: %v", err)
			}
		}()
	}
	wg.Wait()
	if reopened != 1 {
		t.Fatalf("reopened %d times, want exactly 1", reopened)
	}
}
