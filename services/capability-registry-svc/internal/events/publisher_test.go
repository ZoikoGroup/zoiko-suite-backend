// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id). tenant_id is correctly
// omitted here: capabilities and release state are platform-wide reference
// data with no tenant_id anywhere on domain.Capability/domain.Release —
// the empty string this service used to pass explicitly is a real absence
// of data, not an oversight.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/capability-registry-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return f.err
}

type envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesActorAndCorrelationID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.capability-registry.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "capability_release.state_changed", EntityID: "cap-1",
		ActorID: "releaser-1", CorrelationID: "corr-1",
		Payload: map[string]string{"state": "GA"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "capability_release.state_changed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "capability-registry-svc", env.SourceService)
	assert.Equal(t, "releaser-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameCapability_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.capability-registry.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "capability_release.state_changed", EntityID: "cap-1",
			ActorID: "releaser-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestPublishOutbox_PropagatesKafkaFailureAndPreservesPayloadOnRetry(t *testing.T) {
	writeErr := assert.AnError
	w := &fakeWriter{err: writeErr}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.capability-registry.events", zap.NewNop())
	payload := []byte(`{"event_id":"evt-stable","event_type":"capability.created"}`)

	err := p.PublishOutbox(context.Background(), "cap-1", payload)
	require.ErrorIs(t, err, writeErr)
	require.Len(t, w.msgs, 1)

	w.err = nil
	require.NoError(t, p.PublishOutbox(context.Background(), "cap-1", payload))
	require.Len(t, w.msgs, 2)
	assert.Equal(t, w.msgs[0].Value, w.msgs[1].Value)
	assert.Equal(t, w.msgs[0].Key, w.msgs[1].Key)
	assert.Equal(t, payload, w.msgs[1].Value)
	require.Len(t, w.msgs[1].Headers, 1)
	assert.Equal(t, "X-Event-ID", w.msgs[1].Headers[0].Key)
	assert.Equal(t, "evt-stable", string(w.msgs[0].Headers[0].Value))
	assert.Equal(t, string(w.msgs[0].Headers[0].Value), string(w.msgs[1].Headers[0].Value))
}

func TestPublishOutbox_RejectsMissingEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.capability-registry.events", zap.NewNop())

	err := p.PublishOutbox(context.Background(), "cap-1", []byte(`{"event_type":"capability.created"}`))
	require.ErrorContains(t, err, "event_id is required")
	assert.Empty(t, w.msgs)
}
