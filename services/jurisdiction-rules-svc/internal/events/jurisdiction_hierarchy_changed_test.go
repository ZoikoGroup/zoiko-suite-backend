package events

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// REF-01 JurisdictionHierarchyChanged: a move is its own event, naming the old
// and the new parent, the version the move produced and the import it came from.

func hierarchyEnvelope(t *testing.T, j domain.Jurisdiction, prev *string, src *ImportSource) map[string]any {
	t.Helper()
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	ctx := WithBatch(context.Background())
	if err := p.PublishJurisdictionHierarchyChanged(ctx, j, prev, src, "corr-1"); err != nil {
		t.Fatal(err)
	}
	evs := pendingOf(t, ctx)
	if len(evs) != 1 || evs[0].EventType != EventJurisdictionHierarchyChanged || evs[0].PartitionKey != j.JurisdictionID {
		t.Fatalf("pending = %+v, want one %s keyed by the jurisdiction", evs, EventJurisdictionHierarchyChanged)
	}
	var env map[string]any
	if err := json.Unmarshal(evs[0].Payload, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestHierarchyChangedEvent_NamesBothParentsTheVersionAndTheImport(t *testing.T) {
	oldParent, newParent, actor := "p-old", "p-new", "checker-2"
	env := hierarchyEnvelope(t,
		domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB-X", JurisdictionType: "STATE_PROVINCE", ParentJurisdictionID: &newParent,
			RowVersion: 9, CreatedByPrincipalID: "c", UpdatedByPrincipalID: &actor, DataClassification: "PUBLIC"},
		&oldParent,
		&ImportSource{BatchID: "b-1", SourceStandard: "ISO 3166-2", SourceVersion: "2026-1", ImportHash: "abc"})

	if env["event_type"] != "jurisdiction.hierarchy.changed" || env["actor_id"] != "checker-2" || env["correlation_id"] != "corr-1" {
		t.Errorf("envelope = %v", env)
	}
	if v, _ := env["aggregate_version"].(float64); v != 9 {
		t.Errorf("aggregate_version = %v, want 9: a consumer must be able to pin the version the move produced", env["aggregate_version"])
	}
	payload, _ := env["payload"].(map[string]any)
	if payload["jurisdiction_id"] != "j-1" || payload["jurisdiction_code"] != "GB-X" || payload["jurisdiction_type"] != "STATE_PROVINCE" ||
		payload["previous_parent_jurisdiction_id"] != "p-old" || payload["parent_jurisdiction_id"] != "p-new" {
		t.Errorf("payload = %v", payload)
	}
	src, _ := payload["import"].(map[string]any)
	if src["batch_id"] != "b-1" || src["source_standard"] != "ISO 3166-2" || src["source_version"] != "2026-1" || src["import_hash"] != "abc" {
		t.Errorf("import = %v", src)
	}
}

// Moving to or from the root is a real move: a nil parent is carried as null, not dropped.
func TestHierarchyChangedEvent_ARootIsNullNotMissing(t *testing.T) {
	env := hierarchyEnvelope(t, domain.Jurisdiction{JurisdictionID: "j-1", CreatedByPrincipalID: "c", DataClassification: "PUBLIC"}, nil, nil)
	payload, _ := env["payload"].(map[string]any)
	for _, k := range []string{"previous_parent_jurisdiction_id", "parent_jurisdiction_id"} {
		v, present := payload[k]
		if !present || v != nil {
			t.Errorf("%s = %v (present=%v), want an explicit null", k, v, present)
		}
	}
	if _, has := payload["import"]; has {
		t.Error("an import appeared where none was given")
	}
}

func TestUpdatedEvent_ForAnAttributeChangeCarriesBeforeAfterAndTheImport(t *testing.T) {
	env := updatedEnvelope(t,
		domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB", JurisdictionName: "United Kingdom", AuthorityType: "SOVEREIGN", RowVersion: 3, CreatedByPrincipalID: "c", DataClassification: "PUBLIC"},
		JurisdictionChange{Type: ChangeAttributesChanged,
			Before: &domain.Jurisdiction{JurisdictionName: "UK", AuthorityType: "FEDERAL"},
			Import: &ImportSource{BatchID: "b-1", SourceStandard: "ISO 3166-1", SourceVersion: "2026-1", ImportHash: "h"}})
	payload, _ := env["payload"].(map[string]any)
	prev, _ := payload["previous"].(map[string]any)
	cur, _ := payload["current"].(map[string]any)
	if payload["change_type"] != "ATTRIBUTES_CHANGED" || prev["jurisdiction_name"] != "UK" || prev["authority_type"] != "FEDERAL" ||
		cur["jurisdiction_name"] != "United Kingdom" || cur["authority_type"] != "SOVEREIGN" {
		t.Errorf("payload = %v", payload)
	}
	if src, _ := payload["import"].(map[string]any); src["batch_id"] != "b-1" || src["source_version"] != "2026-1" {
		t.Errorf("import = %v", payload["import"])
	}
}

func TestNoopPublisher_AcceptsTheHierarchyEvent(t *testing.T) {
	if err := NewNoopPublisher(zap.NewNop()).PublishJurisdictionHierarchyChanged(context.Background(), domain.Jurisdiction{}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
}
