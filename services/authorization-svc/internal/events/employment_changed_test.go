package events_test

import (
	"context"
	"testing"
)

// Doc 03 §8.3 "Consume employment.changed": access-control-svc publishes a
// linked employee's exit, keyed on the principal; an exit projects the
// principal TERMINATED (layer 0 denies), anything else projects nothing.

func TestLifecycle_EmploymentExitTerminatesThePrincipal(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)
	if err := c.Handle(context.Background(), lifecycleEnvelope(t, "e-1", "employment.changed", "t-1", map[string]any{
		"principal_id": "p-9", "employee_id": "E-1", "tenant_id": "t-1",
		"employment_status": "RESIGNED", "previous_status": "ACTIVE", "status_changed_at": "2026-10-08T09:00:00Z",
	})); err != nil {
		t.Fatal(err)
	}
	if len(store.projected) != 1 {
		t.Fatalf("projected %d, want 1", len(store.projected))
	}
	got := store.projected[0]
	if got.PrincipalID != "p-9" || got.TenantID != "t-1" || got.Status != "TERMINATED" || got.SourceService != "access-control-svc" {
		t.Fatalf("projected %+v", got)
	}
	if got.StatusChangedAt == nil {
		t.Error("status_changed_at must travel, so a stale replay is refused")
	}
}

func TestLifecycle_EmploymentNonExitProjectsNothing(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)
	for i, st := range []string{"ON_LEAVE", "ACTIVE", "SUSPENDED", ""} {
		_ = c.Handle(context.Background(), lifecycleEnvelope(t, "e-n"+string(rune('a'+i)), "employment.changed", "t-1", map[string]any{
			"principal_id": "p-9", "tenant_id": "t-1", "employment_status": st,
		}))
	}
	if len(store.projected) != 0 {
		t.Fatalf("a non-exit projected a status: %+v", store.projected)
	}
}
