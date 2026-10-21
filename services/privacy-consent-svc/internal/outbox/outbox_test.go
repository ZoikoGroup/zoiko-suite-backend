package outbox

import (
	"encoding/json"
	"testing"
)

// TestBuildPayload_RoundTrips pins the on-the-wire envelope shape: the
// fields a consumer migrating from the old events.KafkaPublisher relies on
// (event_type, tenant_id, entity_id, actor_id, correlation_id, payload)
// must still be there, unrenamed, now that delivery goes through the
// outbox instead of a direct post-commit publish.
func TestBuildPayload_RoundTrips(t *testing.T) {
	payload := map[string]string{"consent_receipt_id": "cr-1", "action": "GRANTED"}
	data, err := BuildPayload("privacy.consent.changed", "subject-1", "tenant-1", "principal-1", "corr-1", payload)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}

	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.EventType != "privacy.consent.changed" {
		t.Fatalf("expected event_type privacy.consent.changed, got %s", env.EventType)
	}
	if env.TenantID != "tenant-1" || env.EntityID != "subject-1" || env.ActorID != "principal-1" || env.CorrelationID != "corr-1" {
		t.Fatalf("expected envelope fields to round-trip, got %+v", env)
	}
	if env.SourceService != "privacy-consent-svc" {
		t.Fatalf("expected source_service=privacy-consent-svc, got %s", env.SourceService)
	}
	if env.EventID == "" {
		t.Fatalf("expected a non-empty event_id")
	}

	var decodedPayload map[string]string
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		t.Fatalf("remarshal payload: %v", err)
	}
	if err := json.Unmarshal(payloadBytes, &decodedPayload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if decodedPayload["consent_receipt_id"] != "cr-1" || decodedPayload["action"] != "GRANTED" {
		t.Fatalf("expected payload to round-trip, got %+v", decodedPayload)
	}
}

// TestBuildPayload_EachCallGetsAUniqueEventID guards against a relay or a
// retry treating two distinct facts as the same event because their IDs
// collided.
func TestBuildPayload_EachCallGetsAUniqueEventID(t *testing.T) {
	a, err := BuildPayload("privacy.consent.changed", "subject-1", "tenant-1", "principal-1", "corr-1", nil)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	b, err := BuildPayload("privacy.consent.changed", "subject-1", "tenant-1", "principal-1", "corr-1", nil)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var envA, envB Envelope
	_ = json.Unmarshal(a, &envA)
	_ = json.Unmarshal(b, &envB)
	if envA.EventID == envB.EventID {
		t.Fatalf("expected distinct event_id per call, got the same value twice: %s", envA.EventID)
	}
}
