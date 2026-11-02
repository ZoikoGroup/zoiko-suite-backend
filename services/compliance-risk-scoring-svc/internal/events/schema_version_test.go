package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"zoiko.io/compliance-risk-scoring-svc/internal/events"
)

// audit-event-store-svc rejects an event without schema_version as malformed
// (found 2026-11-02 in the Kafka -> audit run). Both version fields are sent.
func TestBuildEvent_CarriesSchemaVersionForAuditConsumer(t *testing.T) {
	raw, err := json.Marshal(events.BuildEvent(events.PublishParams{EventType: "risk_score.calculated", SubjectID: "r-1", TenantID: "t-1"}, "e-1", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["schema_version"] != "1.0" || m["event_version"] != "1.0" || m["source_service"] == "" || m["event_type"] != "risk_score.calculated" {
		t.Fatalf("envelope missing audit-required fields: %s", raw)
	}
}
