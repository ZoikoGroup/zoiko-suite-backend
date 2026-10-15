package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// envelopeWithEmittedAt is lifecycleEnvelope plus the producers' emitted_at.
func envelopeWithEmittedAt(t *testing.T, eventID, eventType string, emitted time.Time, payload map[string]any) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(lifecycleEnvelope(t, eventID, eventType, "t-1", payload), &env); err != nil {
		t.Fatal(err)
	}
	env["emitted_at"] = emitted.Format(time.RFC3339Nano)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Governance audit N-05: neither producer sends status_changed_at, so the
// stale-event guard never fired and a replayed ACTIVE could re-activate a
// principal suspended after it. The envelope's emitted_at is the fallback.
func TestLifecycle_EmittedAtOrdersStatusWhenPayloadHasNoTime(t *testing.T) {
	emitted := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)
	cases := map[string]map[string]any{
		"principal.status.changed": {"principal_id": "p-9", "tenant_id": "t-1", "new_status": "SUSPENDED"},
		"employment.changed":       {"principal_id": "p-9", "tenant_id": "t-1", "employment_status": "TERMINATED"},
	}
	for eventType, payload := range cases {
		store := &lifecycleStub{}
		c := newLifecycle(store)
		if err := c.Handle(context.Background(), envelopeWithEmittedAt(t, "e-"+eventType, eventType, emitted, payload)); err != nil {
			t.Fatal(err)
		}
		if len(store.projected) != 1 {
			t.Fatalf("%s: projected %d, want 1", eventType, len(store.projected))
		}
		got := store.projected[0].StatusChangedAt
		if got == nil || !got.Equal(emitted) {
			t.Errorf("%s: status_changed_at = %v, want the envelope's emitted_at %v", eventType, got, emitted)
		}
	}
}

// A producer-stated change time still wins over the envelope's.
func TestLifecycle_PayloadTimeWinsOverEmittedAt(t *testing.T) {
	stated := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	store := &lifecycleStub{}
	c := newLifecycle(store)
	if err := c.Handle(context.Background(), envelopeWithEmittedAt(t, "e-p", "principal.status.changed", stated.Add(time.Hour), map[string]any{
		"principal_id": "p-9", "tenant_id": "t-1", "new_status": "SUSPENDED", "status_changed_at": stated.Format(time.RFC3339),
	})); err != nil {
		t.Fatal(err)
	}
	if got := store.projected[0].StatusChangedAt; got == nil || !got.Equal(stated) {
		t.Errorf("status_changed_at = %v, want the payload's %v", got, stated)
	}
}
