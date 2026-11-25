package domain_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// A11: CollectionStateOf's extended billing-health summary (§6: Current,
// PaymentPending, PastDue, Collections, WrittenOff), derived from real data
// this service already records elsewhere (dunning cases, write-offs) rather
// than a second tracked state machine. Pure domain logic — no database.

func dunningCase(status domain.DunningStatus) *domain.DunningCase {
	return &domain.DunningCase{CaseID: "cdun_1", Status: status, OpenedAt: time.Now()}
}

func TestCollectionStateOf_WrittenOffOutranksEverythingIncludingASuccessfulPayment(t *testing.T) {
	attempts := []domain.PaymentAttemptRef{{Status: domain.PaymentSucceeded}}
	got := domain.CollectionStateOf(attempts, nil, true)
	if got != domain.CollectionWrittenOff {
		t.Fatalf("got %s, want WRITTEN_OFF (a recorded write-off outranks a succeeded payment attempt)", got)
	}
}

func TestCollectionStateOf_PaidWinsOverAnOpenEarlyDunningCase(t *testing.T) {
	attempts := []domain.PaymentAttemptRef{{Status: domain.PaymentSucceeded}}
	got := domain.CollectionStateOf(attempts, dunningCase(domain.DunningNotice1), false)
	if got != domain.CollectionPaid {
		t.Fatalf("got %s, want PAID", got)
	}
}

func TestCollectionStateOf_OpenNoticeDunningIsPastDue(t *testing.T) {
	for _, stage := range []domain.DunningStatus{domain.DunningNotice1, domain.DunningNotice2} {
		got := domain.CollectionStateOf(nil, dunningCase(stage), false)
		if got != domain.CollectionPastDue {
			t.Fatalf("dunning %s: got %s, want PAST_DUE", stage, got)
		}
	}
}

func TestCollectionStateOf_RestrictedOrSuspendedDunningIsCollections(t *testing.T) {
	for _, stage := range []domain.DunningStatus{domain.DunningRestricted, domain.DunningSuspended} {
		got := domain.CollectionStateOf(nil, dunningCase(stage), false)
		if got != domain.CollectionCollections {
			t.Fatalf("dunning %s: got %s, want COLLECTIONS", stage, got)
		}
	}
}

// A real invoice can have a dunning case that is already CLOSED (no longer
// "open" at all) while still having an unresolved payment attempt — the
// closed case must not itself produce PAST_DUE/COLLECTIONS; the payment
// attempt still governs.
func TestCollectionStateOf_ClosedDunningCaseDoesNotShadowPaymentPending(t *testing.T) {
	attempts := []domain.PaymentAttemptRef{{Status: domain.PaymentSubmitted}}
	got := domain.CollectionStateOf(attempts, dunningCase(domain.DunningClosed), false)
	if got != domain.CollectionPaymentPending {
		t.Fatalf("got %s, want PAYMENT_PENDING (a CLOSED dunning case carries no billing-health weight)", got)
	}
}

func TestCollectionStateOf_NoAttemptsNoDunningNoWriteOffIsCurrent(t *testing.T) {
	got := domain.CollectionStateOf(nil, nil, false)
	if got != domain.CollectionCurrent {
		t.Fatalf("got %s, want CURRENT", got)
	}
}

func TestCollectionStateOf_UnresolvedAttemptIsPaymentPending(t *testing.T) {
	attempts := []domain.PaymentAttemptRef{{Status: domain.PaymentPendingUnknown}}
	got := domain.CollectionStateOf(attempts, nil, false)
	if got != domain.CollectionPaymentPending {
		t.Fatalf("got %s, want PAYMENT_PENDING", got)
	}
}

func TestCollectionStateOf_OnlyFailedAttemptsWithNoDunningOrWriteOffIsFailed(t *testing.T) {
	attempts := []domain.PaymentAttemptRef{{Status: domain.PaymentFailed}}
	got := domain.CollectionStateOf(attempts, nil, false)
	if got != domain.CollectionFailed {
		t.Fatalf("got %s, want FAILED", got)
	}
}
