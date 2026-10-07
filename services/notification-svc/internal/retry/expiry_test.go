package retry_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/retry"
)

// NCD-015: a communication past its expires_at is never handed to a provider, even when it is
// due and was claimed: the submit gate checks.
func TestWorkerNeverSubmitsAnExpiredCommunication(t *testing.T) {
	s := newStubStore()
	seed(s, "n-expired", "tenant-a", 0)
	past := time.Now().UTC().Add(-time.Minute)
	s.byID["n-expired"].ExpiresAt = &past
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true}}

	newWorker(s, d, &stubPublisher{}, nil, retry.DefaultPolicy).RunOnce(context.Background())

	if d.calls != 0 {
		t.Fatalf("an expired communication reached the provider %d times", d.calls)
	}
	if len(s.expired) != 1 || s.expired[0] != "n-expired" {
		t.Fatalf("expired = %v, want [n-expired]", s.expired)
	}
	if len(s.completed) != 0 {
		t.Fatalf("an expiry is not a delivery conclusion: %v", s.completed)
	}
}

// The sweep half: queued communications past expiry are concluded without being claimed.
func TestWorkerExpiresOverdueQueuedCommunications(t *testing.T) {
	s := newStubStore()
	past := time.Now().UTC().Add(-time.Hour)
	s.byID["q1"] = &domain.Notification{NotificationID: "q1", TenantID: "tenant-a", Status: domain.StatusPending, ExpiresAt: &past}
	future := time.Now().UTC().Add(time.Hour)
	s.byID["q2"] = &domain.Notification{NotificationID: "q2", TenantID: "tenant-a", Status: domain.StatusPending, ExpiresAt: &future}
	s.overdue = []domain.DueRetry{{NotificationID: "q1", TenantID: "tenant-a"}, {NotificationID: "q2", TenantID: "tenant-a"}}

	w := newWorker(s, &stubDeliverer{}, &stubPublisher{}, nil, retry.DefaultPolicy)
	if got := w.ExpireOverdue(context.Background()); got != 1 {
		t.Fatalf("expired %d, want 1 (only the overdue one)", got)
	}
	if len(s.expired) != 1 || s.expired[0] != "q1" {
		t.Fatalf("expired = %v, want [q1]", s.expired)
	}
}
