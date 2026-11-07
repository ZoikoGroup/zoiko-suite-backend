package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validEvent() OrgEvent {
	return OrgEvent{
		Name: EventOrgUnitReparented, TenantID: "tenant-a", LegalEntityID: "le-1", UnitID: "unit-1",
		ObjectType: ObjectOrgUnit, ObjectID: "unit-1", ObjectVersion: 3, EffectiveAt: "2026-03-01",
		RecordedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("IST", 19800)),
		Actor:      "alice", CorrelationID: "corr-1", EvidenceRef: "req-9",
		Details: map[string]any{"new_parent_unit_id": "unit-2"},
	}
}

// The spec's event minimum (ZS-SVC-I-001 s7): tenant_id, object_id,
// object_version, effective_at, recorded_at, actor, correlation_id and, where
// material, evidence_ref. All of them are in the payload.
func TestBuildOrgEvent_CarriesTheSpecMinimum(t *testing.T) {
	rec, err := BuildOrgEvent(validEvent())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rec.EventType != EventOrgUnitReparented || rec.TenantID != "tenant-a" || rec.PartitionKey != "unit-1" {
		t.Fatalf("record: %+v", rec)
	}

	var env struct {
		EventID       string          `json:"event_id"`
		EventType     string          `json:"event_type"`
		SourceService string          `json:"source_service"`
		TenantID      string          `json:"tenant_id"`
		ActorID       string          `json:"actor_id"`
		CorrelationID string          `json:"correlation_id"`
		EmittedAt     time.Time       `json:"emitted_at"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(rec.Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.EventType != "org.unit.reparented" || env.SourceService != "org-structure-svc" || env.TenantID != "tenant-a" ||
		env.ActorID != "alice" || env.CorrelationID != "corr-1" {
		t.Fatalf("envelope: %+v", env)
	}
	if env.EventID != "evt-"+rec.EventID {
		t.Fatalf("the envelope event_id must be the outbox id, so consumers dedupe on one value: %s vs %s", env.EventID, rec.EventID)
	}

	var p map[string]any
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tenant_id", "object_id", "object_version", "effective_at", "recorded_at", "actor", "correlation_id", "evidence_ref"} {
		if _, ok := p[field]; !ok {
			t.Errorf("payload is missing the spec field %q: %v", field, p)
		}
	}
	if p["object_version"].(float64) != 3 || p["effective_at"] != "2026-03-01" || p["evidence_ref"] != "req-9" {
		t.Fatalf("payload values: %v", p)
	}
	// recorded_at is UTC whatever zone the caller used, so consumers compare it directly.
	if !strings.HasSuffix(p["recorded_at"].(string), "Z") {
		t.Fatalf("recorded_at must be UTC, got %v", p["recorded_at"])
	}
	if d, ok := p["details"].(map[string]any); !ok || d["new_parent_unit_id"] != "unit-2" {
		t.Fatalf("details: %v", p["details"])
	}
}

func TestBuildOrgEvent_OmitsEvidenceWhenThereIsNone(t *testing.T) {
	ev := validEvent()
	ev.EvidenceRef, ev.Details = "", nil
	rec, err := BuildOrgEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	_ = json.Unmarshal(rec.Payload, &env)
	if _, present := env.Payload["evidence_ref"]; present {
		t.Fatal("evidence_ref is only carried where material")
	}
	if d, ok := env.Payload["details"].(map[string]any); !ok || len(d) != 0 {
		t.Fatalf("details is always an object, never null: %v", env.Payload["details"])
	}
}

// A command must not be able to commit an event a consumer could not order or
// attribute.
func TestBuildOrgEvent_RefusesIncompleteEvents(t *testing.T) {
	cases := map[string]func(*OrgEvent){
		"no name":          func(e *OrgEvent) { e.Name = "" },
		"no tenant":        func(e *OrgEvent) { e.TenantID = "" },
		"no object id":     func(e *OrgEvent) { e.ObjectID = "" },
		"no unit id":       func(e *OrgEvent) { e.UnitID = "" },
		"version zero":     func(e *OrgEvent) { e.ObjectVersion = 0 },
		"no effective":     func(e *OrgEvent) { e.EffectiveAt = "" },
		"negative version": func(e *OrgEvent) { e.ObjectVersion = -1 },
	}
	for name, mutate := range cases {
		ev := validEvent()
		mutate(&ev)
		if _, err := BuildOrgEvent(ev); err == nil {
			t.Errorf("%s: an incomplete event must be refused", name)
		}
	}
}

func TestBuildOrgEvent_EveryEventGetsAFreshID(t *testing.T) {
	a, _ := BuildOrgEvent(validEvent())
	b, _ := BuildOrgEvent(validEvent())
	if a.EventID == b.EventID {
		t.Fatal("event ids must be unique per event")
	}
}

func TestOrgEventNamesAreUniqueAndComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range OrgEventNames {
		if seen[n] {
			t.Errorf("duplicate event name %s", n)
		}
		seen[n] = true
	}
	// The four events the spec names for ORG-05 must be among them.
	for _, n := range []string{"org.unit.created", "org.unit.reparented", "org.unit.manager.changed", "org.unit.deactivated"} {
		if !seen[n] {
			t.Errorf("spec event %s is missing", n)
		}
	}
}
