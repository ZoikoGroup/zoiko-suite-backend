package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// REF-01 JurisdictionUpdated: jurisdiction.updated names the change, carries the
// mapping and a version a consumer can pin, and is attributed to who did it.

func updatedEnvelope(t *testing.T, j domain.Jurisdiction, c JurisdictionChange) map[string]any {
	t.Helper()
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	ctx := WithBatch(context.Background())
	if err := p.PublishJurisdictionUpdated(ctx, j, c, "corr-1"); err != nil {
		t.Fatal(err)
	}
	evs := pendingOf(t, ctx)
	if len(evs) != 1 || evs[0].EventType != EventJurisdictionUpdated || evs[0].PartitionKey != j.JurisdictionID {
		t.Fatalf("pending = %+v, want one %s keyed by the jurisdiction", evs, EventJurisdictionUpdated)
	}
	var env map[string]any
	if err := json.Unmarshal(evs[0].Payload, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestUpdatedEvent_ForAnAddedAliasCarriesTheMappingAndTheVersion(t *testing.T) {
	to := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	actor := "steward-9"
	env := updatedEnvelope(t,
		domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB", ActiveFlag: true, RowVersion: 7,
			CreatedByPrincipalID: "creator", UpdatedByPrincipalID: &actor, DataClassification: "PUBLIC"},
		JurisdictionChange{Type: ChangeAliasAdded, Alias: &domain.JurisdictionAlias{
			AliasID: "a-1", AliasSystem: "ISO3166-1-ALPHA3", AliasCode: "GBR", SourceStandard: "ISO 3166-1",
			SourceVersion: "2024-06", MappingRationale: "assigned by the standard", ValidFrom: to.AddDate(-5, 0, 0), ValidTo: &to}})

	if env["event_type"] != "jurisdiction.updated" || env["actor_id"] != "steward-9" || env["correlation_id"] != "corr-1" {
		t.Errorf("envelope = %v", env)
	}
	payload, _ := env["payload"].(map[string]any)
	if payload["change_type"] != "ALIAS_ADDED" || payload["jurisdiction_id"] != "j-1" || payload["jurisdiction_code"] != "GB" {
		t.Errorf("payload = %v", payload)
	}
	alias, _ := payload["alias"].(map[string]any)
	if alias["alias_system"] != "ISO3166-1-ALPHA3" || alias["alias_code"] != "GBR" || alias["source_version"] != "2024-06" ||
		alias["mapping_rationale"] != "assigned by the standard" || alias["alias_id"] != "a-1" {
		t.Errorf("alias = %v", alias)
	}
	if v, _ := env["aggregate_version"].(float64); v != 7 {
		t.Errorf("aggregate_version = %v, want 7: a consumer must be able to pin the version", env["aggregate_version"])
	}
}

func TestUpdatedEvent_WithoutAnAliasStillPublishes(t *testing.T) {
	env := updatedEnvelope(t, domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB", CreatedByPrincipalID: "c", DataClassification: "PUBLIC"},
		JurisdictionChange{Type: "ATTRIBUTES_CHANGED"})
	payload, _ := env["payload"].(map[string]any)
	if payload["change_type"] != "ATTRIBUTES_CHANGED" {
		t.Errorf("payload = %v", payload)
	}
	if _, has := payload["alias"]; has {
		t.Errorf("an alias appeared where none was given: %v", payload)
	}
}
