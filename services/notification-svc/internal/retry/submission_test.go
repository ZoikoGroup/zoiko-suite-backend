package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/retry"
)

// §6.2: "UNKNOWN never authorizes an immediate blind second material send."
// A stranded row that was handed to a provider (Submitted) must become
// PENDING_UNKNOWN, never be revived and re-sent.
func TestSweep_SubmittedStrandedRowIsMarkedUnknownNotRevived(t *testing.T) {
	s := newStubStore()
	s.byID["n-sub"] = &domain.Notification{NotificationID: "n-sub", TenantID: "tenant-a", Status: "PENDING"}
	s.byID["n-new"] = &domain.Notification{NotificationID: "n-new", TenantID: "tenant-a", Status: "PENDING"}
	s.stranded = []domain.DueRetry{
		{NotificationID: "n-sub", TenantID: "tenant-a", Submitted: true},
		{NotificationID: "n-new", TenantID: "tenant-a"},
	}
	pub := &stubPublisher{}
	s.events = pub

	newSweepWorker(s, 15*time.Minute).SweepStranded(context.Background())

	if s.revived["n-sub"] {
		t.Fatal("a submitted stranded row was revived — that is the blind re-send §6.2 forbids")
	}
	if len(s.markedUnknown) != 1 || s.markedUnknown[0] != "n-sub" {
		t.Fatalf("markedUnknown = %v, want [n-sub]", s.markedUnknown)
	}
	if !s.revived["n-new"] {
		t.Fatal("a never-submitted stranded row must still be revived")
	}
	if pub.unknown != 1 {
		t.Fatalf("outcome_unknown events = %d, want 1", pub.unknown)
	}
}

// The submission is marked before the provider is called, and if the mark
// cannot be written the provider is not called at all.
func TestWorker_MarksSubmissionBeforeCallingTheProvider(t *testing.T) {
	s := newStubStore()
	seed(s, "n-1", "tenant-a", 1)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true, ProviderResponse: "250"}}
	newWorker(s, d, &stubPublisher{}, nil, retry.DefaultPolicy).RunOnce(context.Background())
	if len(s.submitted) != 1 || s.submitted[0] != "n-1" || d.calls != 1 {
		t.Fatalf("submitted=%v calls=%d, want [n-1] and 1", s.submitted, d.calls)
	}

	s2 := newStubStore()
	seed(s2, "n-2", "tenant-a", 1)
	s2.beginSubmissionErr = errors.New("db down")
	d2 := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true}}
	newWorker(s2, d2, &stubPublisher{}, nil, retry.DefaultPolicy).RunOnce(context.Background())
	if d2.calls != 0 {
		t.Fatalf("the provider was called %d times although the submission could not be marked", d2.calls)
	}
}
