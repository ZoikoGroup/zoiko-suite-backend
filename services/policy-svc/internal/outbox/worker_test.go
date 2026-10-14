package outbox

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Audit row "Outbox after commit; at-least-once delivery": the message must
// be one kafka-go accepts from a Writer that owns the topic, carry the outbox
// row's own event id (the same on every redelivery), be keyed by aggregate so
// a family's events stay ordered, and hash the exact payload bytes.
func TestMessage_EnvelopeIsStableAndComplete(t *testing.T) {
	payload := []byte(`{"policy_id":"p1","version_status":"ACTIVE"}`)
	e := Event{
		EventID: "4f0c6c39-6c3a-4f39-9f5e-111111111111", EventType: "policy.version.activated",
		EventVersion: "1.0", SchemaVersion: "1.0", SourceService: "policy-svc",
		TenantID: "t1", ActorID: "approver", CorrelationID: "corr", AggregateID: "p1",
		AggregateVersion: 4, IdempotencyKey: "ik", Payload: payload,
		OccurredAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
	}
	m1, err := Message(e)
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := Message(e)

	if m1.Topic != "" {
		t.Fatal("the message must not name a topic: kafka-go refuses one when the Writer has its own")
	}
	if string(m1.Key) != "p1" {
		t.Fatalf("expected the aggregate id as key, got %q", m1.Key)
	}
	var env1, env2 map[string]any
	_ = json.Unmarshal(m1.Value, &env1)
	_ = json.Unmarshal(m2.Value, &env2)
	if env1["event_id"] != e.EventID || env2["event_id"] != e.EventID {
		t.Fatal("every delivery of a row must carry the row's event_id")
	}
	if env1["payload_hash"] != fmt.Sprintf("sha256:%x", sha256.Sum256(payload)) {
		t.Fatalf("payload_hash must be over the exact payload bytes, got %v", env1["payload_hash"])
	}
	for _, f := range []string{"classification", "aggregate_version", "causation_id", "actor_id", "tenant_id", "correlation_id", "emitted_at"} {
		if env1[f] == nil || env1[f] == "" {
			t.Errorf("envelope field %s missing", f)
		}
	}
	if env1["aggregate_version"] != "4" || env1["emitted_at"] != "2026-10-14T00:00:00Z" {
		t.Errorf("unexpected aggregate_version/emitted_at: %v %v", env1["aggregate_version"], env1["emitted_at"])
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if backoff(1) != time.Second || backoff(100) != maxBackoff {
		t.Fatalf("unexpected backoff %v %v", backoff(1), backoff(100))
	}
}
