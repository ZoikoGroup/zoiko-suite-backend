package events_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/events"
)

// The audit's scenario: a suspension or an upstream revocation arrives while
// the database is down. Handle must report the failure AND leave the event
// retryable — it used to claim the event id first, so the retry was skipped as
// "already handled" and the principal kept full authority.

func TestLifecycle_SuspensionFailedApplyIsAppliedOnRetry(t *testing.T) {
	store := &lifecycleStub{projectErr: errors.New("db down")}
	c := newLifecycle(store)
	raw := lifecycleEnvelope(t, "evt-susp-1", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-1", "tenant_id": "t-1", "new_status": "SUSPENDED",
	})

	if err := c.Handle(context.Background(), raw); err == nil {
		t.Fatal("Handle reported success for a suspension the store refused")
	}
	store.projectErr = nil
	if err := c.Handle(context.Background(), raw); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(store.projected) != 1 || store.projected[0].Status != "SUSPENDED" {
		t.Fatalf("suspension not applied on retry: %+v", store.projected)
	}
}

func TestDelegation_RevocationFailedApplyIsAppliedOnRetry(t *testing.T) {
	store := &fakeProjector{revokeErr: errors.New("db down")}
	c := events.NewConsumer(zap.NewNop(), store)
	raw := []byte(strings.Replace(delegatedEvent, `"authority.delegated"`, `"authority.revoked"`, 1))

	if err := c.Handle(context.Background(), raw); err == nil {
		t.Fatal("Handle reported success for a revocation the store refused")
	}
	store.revokeErr = nil
	if err := c.Handle(context.Background(), raw); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(store.revoked) != 1 {
		t.Fatalf("revocation not applied on retry: %+v", store.revoked)
	}
}

// A successful apply is still deduplicated: the claim is released only on failure.
func TestLifecycle_SuccessfulApplyStillDeduplicated(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)
	raw := lifecycleEnvelope(t, "evt-susp-2", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-1", "tenant_id": "t-1", "new_status": "SUSPENDED",
	})
	_ = c.Handle(context.Background(), raw)
	_ = c.Handle(context.Background(), raw)
	if len(store.projected) != 1 {
		t.Fatalf("redelivered event applied %d times, want 1", len(store.projected))
	}
}
