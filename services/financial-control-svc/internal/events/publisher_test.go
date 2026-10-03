package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/outbox"
)

type captureWriter struct{ msgs []kafka.Message }

func (c *captureWriter) WriteMessages(_ context.Context, m ...kafka.Message) error {
	c.msgs = append(c.msgs, m...)
	return nil
}

// auditStoreView mirrors, field for field, the envelope audit-event-store-svc's consumer parses
// (services/audit-event-store-svc/internal/consumer/consumer.go). If this decode breaks, the
// platform's evidence store can no longer read this service's events.
type auditStoreView struct {
	EventType     string          `json:"event_type"`
	EmittedAt     string          `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

func delivery() outbox.Delivery {
	return outbox.Delivery{
		OutboxEventID: "0193a1b2-0000-7000-8000-000000000001", AggregateType: "control_run", AggregateID: "run-1",
		EventType: RunCertified, CorrelationID: "corr-1", TenantID: "t1", LegalEntityID: "e1", ActorID: "checker",
		OccurredAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), Payload: []byte(`{"run_id":"run-1","version":7}`)}
}

func TestEnvelopeStaysReadableByThePlatformConsumers(t *testing.T) {
	w := &captureWriter{}
	require.NoError(t, NewPublisher(zap.NewNop(), "topic", w).PublishOutbox(context.Background(), delivery()))
	require.Len(t, w.msgs, 1)

	var v auditStoreView
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &v))
	assert.Equal(t, RunCertified, v.EventType, "the event type keeps the platform's short dotted form")
	assert.Equal(t, "2026-09-01T10:00:00Z", v.EmittedAt, "emitted_at is when the fact was committed, not relay time")
	assert.Equal(t, "financial-control-svc", v.SourceService)
	assert.Equal(t, "1.0", v.SchemaVersion)
	assert.Equal(t, "corr-1", v.CorrelationID)
	assert.Equal(t, "t1", v.TenantID)
	assert.JSONEq(t, `{"run_id":"run-1","version":7}`, string(v.Payload))
}

func TestEnvelopeCarriesTheAdditiveZSEvent001Attributes(t *testing.T) {
	w := &captureWriter{}
	p := NewPublisher(zap.NewNop(), "topic", w).WithProfile("eu-west", "FINANCIAL_CONFIDENTIAL")
	require.NoError(t, p.PublishOutbox(context.Background(), delivery()))

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &got))
	assert.Equal(t, "0193a1b2-0000-7000-8000-000000000001", got["event_id"])
	assert.Equal(t, "control_run", got["aggregate_type"])
	assert.Equal(t, "run-1", got["aggregate_id"])
	assert.Equal(t, float64(7), got["aggregate_version"])
	assert.Equal(t, "eu-west", got["residency_region"])
	assert.Equal(t, "FINANCIAL_CONFIDENTIAL", got["classification"])
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, got["payload_hash"])
	assert.NotEmpty(t, got["published_at"])
	assert.NotContains(t, got, "specversion", "CloudEvents naming is a platform-wide decision, not applied here")
	assert.Equal(t, PartitionKey("t1", "run-1"), w.msgs[0].Key)
	assert.Equal(t, "0193a1b2-0000-7000-8000-000000000001", string(w.msgs[0].Headers[0].Value))
}

func TestPartitionKeyIsStableOpaqueAndPerAggregate(t *testing.T) {
	a := PartitionKey("t1", "run-1")
	assert.Equal(t, a, PartitionKey("t1", "run-1"))
	assert.NotEqual(t, a, PartitionKey("t1", "run-2"), "different aggregates spread across partitions")
	assert.NotEqual(t, a, PartitionKey("t2", "run-1"))
	assert.NotContains(t, string(a), "t1", "the key is opaque: it does not leak tenant or aggregate ids")
}

func TestProfileDefaultsToUnspecifiedNotBlank(t *testing.T) {
	w := &captureWriter{}
	d := delivery()
	d.Payload = []byte(`{}`)
	require.NoError(t, NewPublisher(zap.NewNop(), "t", w).PublishOutbox(context.Background(), d))
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &got))
	assert.Equal(t, Unspecified, got["residency_region"])
	assert.Equal(t, Unspecified, got["classification"])
	assert.NotContains(t, got, "aggregate_version", "no version in the payload means no ordering claim")
}
