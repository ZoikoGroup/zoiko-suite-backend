package events_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/authorization-svc/internal/events"
)

// identity-context-svc's identity.subject.merged payload, as
// internal/lifecycle.Service.Merge renders it.
func mergedPayload(survivor, duplicate, tenant string) map[string]any {
	return map[string]any{"survivor_id": survivor, "duplicate_id": duplicate, "principal_id": duplicate,
		"tenant_id": tenant, "new_status": "DISABLED", "actor": "P-ADMIN", "reason": "merged into " + survivor}
}

func TestLifecycle_SubjectMergedRepointsAssignments(t *testing.T) {
	stub := &lifecycleStub{}
	c := newLifecycle(stub)
	if !events.LifecycleConsumedEventTypes()["identity.subject.merged"] {
		t.Fatal("identity.subject.merged is not subscribed")
	}

	err := c.Handle(context.Background(), lifecycleEnvelope(t, "evt-m1", "identity.subject.merged", "t-1",
		mergedPayload("P-SURV", "P-DUP", "t-1")))
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.merges) != 1 {
		t.Fatalf("merges: %+v", stub.merges)
	}
	m := stub.merges[0]
	if m.SurvivorID != "P-SURV" || m.DuplicateID != "P-DUP" || m.TenantID != "t-1" || m.Actor != "P-ADMIN" {
		t.Fatalf("merge params: %+v", m)
	}
	if len(stub.invalidatedTenants) != 1 || stub.invalidatedTenants[0] != "t-1" {
		t.Fatalf("grant sources not invalidated for the tenant: %v", stub.invalidatedTenants)
	}

	// A redelivery of the same event is not applied twice by this process.
	if err := c.Handle(context.Background(), lifecycleEnvelope(t, "evt-m1", "identity.subject.merged", "t-1",
		mergedPayload("P-SURV", "P-DUP", "t-1"))); err != nil || len(stub.merges) != 1 {
		t.Fatalf("redelivery applied again: err %v merges %d", err, len(stub.merges))
	}
}

func TestLifecycle_SubjectMergedRefusesAGuess(t *testing.T) {
	for name, p := range map[string]map[string]any{
		"no survivor":     mergedPayload("", "P-DUP", "t-1"),
		"no duplicate":    mergedPayload("P-SURV", "", "t-1"),
		"merge into self": mergedPayload("P-X", "P-X", "t-1"),
	} {
		stub := &lifecycleStub{}
		if err := newLifecycle(stub).Handle(context.Background(), lifecycleEnvelope(t, "evt-"+name, "identity.subject.merged", "t-1", p)); err != nil {
			t.Fatalf("%s: a malformed event is committed, not retried: %v", name, err)
		}
		if len(stub.merges) != 0 {
			t.Fatalf("%s: re-pointed on a guess: %+v", name, stub.merges)
		}
	}
}

func TestLifecycle_SubjectMergedStoreFailureIsRetried(t *testing.T) {
	stub := &lifecycleStub{mergeErr: errors.New("db down")}
	c := newLifecycle(stub)
	raw := lifecycleEnvelope(t, "evt-m2", "identity.subject.merged", "t-1", mergedPayload("P-SURV", "P-DUP", "t-1"))
	if err := c.Handle(context.Background(), raw); err == nil {
		t.Fatal("a failed re-point must be returned so Run retries it")
	}
	stub.mergeErr = nil
	if err := c.Handle(context.Background(), raw); err != nil || len(stub.merges) != 1 {
		t.Fatalf("the retry was skipped as already handled: err %v merges %d", err, len(stub.merges))
	}
}
