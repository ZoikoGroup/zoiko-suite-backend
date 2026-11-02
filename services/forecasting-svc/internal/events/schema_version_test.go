package events_test

import (
	"encoding/json"
	"testing"

	"zoiko.io/forecasting-svc/internal/events"
)

// audit-event-store-svc rejects an event without schema_version as malformed
// (found 2026-11-02 in the Kafka -> audit run). Both version fields are sent.
func TestBuildEvent_CarriesSchemaVersionForAuditConsumer(t *testing.T) {
	raw, err := json.Marshal(events.BuildEvent(events.PublishParams{EventType: "forecast.created", SubjectID: "f-1", TenantID: "t-1"}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["schema_version"] != "1.0" || m["event_version"] != "1.0" || m["source_service"] == "" || m["event_type"] != "forecast.created" {
		t.Fatalf("envelope missing audit-required fields: %s", raw)
	}
}
