package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// Integration tests for durable delivery attempts (migration 000011).
//
// §3.4: every provider submission has its own durable attempt_id. Before 000011
// the direct-send path kept a counter and the LAST attempt's reason and
// response, each overwritten by the next, so a notice delivered on its second
// try had no record of what the first was told.

func TestAttempts_RetryThenSuccessRecordsTheChain(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-attempts-chain")

	first := time.Now().UTC().Add(-time.Minute)
	if err := s.ScheduleRetry(ctx, n.NotificationID, "tenant-a", "421 try again later", first, time.Now().UTC(),
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest, ProviderName: "smtp-primary"}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	second := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 queued as Q2", &second, "c",
		domain.AttemptMeta{Origin: domain.AttemptOriginRetry, ProviderName: "smtp-secondary"}); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	got, err := s.ListAttempts(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(got), got)
	}
	a1, a2 := got[0], got[1]
	if a1.AttemptNumber != 1 || a1.Outcome != domain.AttemptOutcomeRetrying || a1.FailureReason != "421 try again later" ||
		!a1.Retryable || a1.ProviderName != "smtp-primary" || a1.Origin != domain.AttemptOriginRequest {
		t.Errorf("first attempt = %+v", a1)
	}
	if a2.AttemptNumber != 2 || a2.Outcome != domain.AttemptOutcomeAccepted || a2.ProviderResponse != "250 queued as Q2" ||
		a2.ProviderName != "smtp-secondary" || a2.Origin != domain.AttemptOriginRetry {
		t.Errorf("second attempt = %+v", a2)
	}
	// The first attempt's evidence survived the second — the whole point.
	if a1.FailureReason == "" {
		t.Error("the first attempt's reason was overwritten")
	}
}

func TestAttempts_UnknownOutcomeIsRecordedAsUnknown(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-attempts-unknown")

	if err := s.MarkOutcomeUnknown(ctx, n.NotificationID, "tenant-a", "connection reset after DATA", time.Now().UTC(), "c",
		domain.AttemptMeta{ProviderName: "smtp-primary"}); err != nil {
		t.Fatalf("MarkOutcomeUnknown: %v", err)
	}
	got, err := s.ListAttempts(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != domain.AttemptOutcomeUnknown || got[0].Origin != domain.AttemptOriginRequest {
		t.Fatalf("attempts = %+v, want one UNKNOWN request attempt", got)
	}
}

// A transition that does not happen records no attempt: a racing replica that
// affects zero rows must not add an attempt to someone else's chain.
func TestAttempts_ARaceLoserRecordsNothing(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-attempts-race")

	at := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "ok", &at, "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.CompleteDelivery(ctx, n.NotificationID, "FAILED", "late", "", &at, "c", domain.AttemptMeta{}); !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("second: want not-found, got %v", err)
	}
	got, _ := s.ListAttempts(ctx, n.NotificationID)
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want 1", len(got))
	}
}

// Attempt rows are tenant-isolated: another tenant reads none.
func TestAttempts_TenantIsolated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := seedNotification(t, s, "tenant-a", "corr-attempts-rls")
	at := time.Now().UTC()
	if err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID, "SENT", "", "ok", &at, "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := s.ListAttempts(tenantCtx("tenant-b"), n.NotificationID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tenant-b read %d of tenant-a's attempts", len(got))
	}
}
