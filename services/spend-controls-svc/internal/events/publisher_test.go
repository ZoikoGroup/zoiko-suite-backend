// Package events_test asserts the event envelope carries the fields Doc 03
// §19 requires, now that the events are built for the outbox.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	"zoiko.io/spend-controls-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	EventVersion  string         `json:"event_version"`
	SchemaVersion string         `json:"schema_version"`
	SourceService string         `json:"source_service"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	ActorID       string         `json:"actor_id"`
	CorrelationID string         `json:"correlation_id"`
	EmittedAt     string         `json:"emitted_at"`
	Payload       map[string]any `json:"payload"`
}

func decodeEnv(t *testing.T, payload any) envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var env envelope
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

func TestBlockedEvents_EnvelopeCarriesTenantLegalEntityActorAndPayload(t *testing.T) {
	in := domain.SpendEvaluation{LegalEntityID: "entity-1", Category: "IT", Amount: 700, CurrencyCode: "GBP", SourceReference: "src-1", PrincipalID: "submitter-1"}
	evs := events.BlockedEvents("tenant-1", "corr-1", in, domain.SpendPolicy{SpendPolicyID: "policy-1", TenantID: "tenant-1", ThresholdAmount: 500}, 900)
	require.Len(t, evs, 2)

	for i, want := range []string{"spend.threshold.breached", "spend.block.applied"} {
		ev := evs[i]
		assert.Equal(t, want, ev.EventType)
		assert.Equal(t, "policy-1", ev.AggregateID)
		assert.Equal(t, "tenant-1", ev.TenantID)
		env := decodeEnv(t, ev.Payload)
		assert.Equal(t, want, env.EventType)
		assert.Equal(t, "1.0", env.EventVersion)
		assert.Equal(t, "1.0", env.SchemaVersion)
		assert.Equal(t, "spend-controls-svc", env.SourceService)
		assert.Equal(t, "tenant-1", env.TenantID)
		assert.Equal(t, "entity-1", env.LegalEntityID)
		assert.Equal(t, "submitter-1", env.ActorID)
		assert.Equal(t, "corr-1", env.CorrelationID)
		assert.Equal(t, "evt-"+ev.OutboxEventID, env.EventID)
		assert.NotEmpty(t, env.EmittedAt)
		assert.Equal(t, "policy-1", env.Payload["spend_policy_id"])
	}
	breached := decodeEnv(t, evs[0].Payload)
	assert.Equal(t, 900.0, breached.Payload["projected_total"])
	assert.Equal(t, 500.0, breached.Payload["threshold_amount"])
	assert.Equal(t, 700.0, breached.Payload["attempted_amount"])
	applied := decodeEnv(t, evs[1].Payload)
	assert.Equal(t, "src-1", applied.Payload["source_reference"])
	assert.NotEmpty(t, applied.Payload["blocked_at"])
	assert.NotEqual(t, evs[0].OutboxEventID, evs[1].OutboxEventID)
}

func TestPublishOutbox_KeyAndEventIDHeader_ErrorReturned(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.spend-controls.events", w)
	require.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "policy-1", []byte(`{}`)))
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "policy-1", string(w.msgs[0].Key))
	assert.Equal(t, "X-Event-ID", w.msgs[0].Headers[0].Key)
	assert.Equal(t, "ob-1", string(w.msgs[0].Headers[0].Value))

	w.err = errors.New("broker down")
	assert.Error(t, p.PublishOutbox(context.Background(), "ob-2", "policy-1", []byte(`{}`)), "a failed write must be returned so the relay retries")
}

func TestPublishOutbox_DryMode_NoProducer(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "t", nil)
	assert.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "policy-1", []byte(`{}`)))
}
