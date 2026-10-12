package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// entity.status.changed is projected, not only used to invalidate the cache:
// /v1/authorize denies in a SUSPENDED or DISSOLVED entity.
func entityStatusEvent(t *testing.T, eventID, status string, at time.Time) []byte {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": "entity.status.changed", "tenant_id": "t-1",
		"legal_entity_id": "e-1", "effective_at": at,
		"payload": map[string]any{"tenant_id": "t-1", "legal_entity_id": "e-1", "previous_status": "ACTIVE", "new_status": status},
	})
	return raw
}

func TestLifecycle_EntityStatusProjected(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	if err := c.Handle(context.Background(), entityStatusEvent(t, "e-ev-1", "DISSOLVED", at)); err != nil {
		t.Fatal(err)
	}
	if len(store.entities) != 1 {
		t.Fatalf("projected %d entity rows, want 1", len(store.entities))
	}
	got := store.entities[0]
	if got.LegalEntityID != "e-1" || got.TenantID != "t-1" || got.Status != "DISSOLVED" || !got.StatusChangedAt.Equal(at) {
		t.Errorf("projection = %+v", got)
	}
	if len(store.invalidatedTenants) != 1 {
		t.Error("cached grants not invalidated alongside the projection")
	}
}

// A failed projection is returned for retry and leaves the event re-applicable.
func TestLifecycle_EntityStatusFailureIsRetryable(t *testing.T) {
	store := &lifecycleStub{entityErr: errors.New("db down")}
	c := newLifecycle(store)
	raw := entityStatusEvent(t, "e-ev-2", "SUSPENDED", time.Now())
	if err := c.Handle(context.Background(), raw); err == nil {
		t.Fatal("a failed entity projection reported success")
	}
	store.entityErr = nil
	if err := c.Handle(context.Background(), raw); err != nil || len(store.entities) != 1 {
		t.Fatalf("retry did not apply: err=%v rows=%d", err, len(store.entities))
	}
}
