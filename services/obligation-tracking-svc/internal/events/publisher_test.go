// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, legal_entity_id, actor_id, correlation_id).
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/obligation-tracking-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.obligation-tracking.events", zap.NewNop())

	err := publish(p, events.PublishParams{
		EventType: "obligation.fulfilled", ObligationID: "obligation-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "fulfiller-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "FULFILLED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "obligation.fulfilled", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "obligation-tracking-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "fulfiller-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameObligation_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.obligation-tracking.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := publish(p, events.PublishParams{
			EventType: "obligation.updated", ObligationID: "obligation-1", TenantID: "tenant-1",
			ActorID: "editor-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

// publish builds the envelope exactly as the store does when it writes the
// outbox row, then delivers it the way the outbox relay does.
func publish(p *events.KafkaPublisher, params events.PublishParams) error {
	id := uuid.NewString()
	body, err := json.Marshal(events.NewEvent(id, params))
	if err != nil {
		return err
	}
	return p.PublishOutbox(context.Background(), id, params.ObligationID, body)
}

type failingWriter struct{}

func (failingWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return errors.New("broker down")
}

// A broker failure is returned to the relay (which keeps the row and
// retries); the old Publish logged it and returned nil, losing the event.
func TestPublishOutbox_ReturnsBrokerError(t *testing.T) {
	p := events.NewKafkaPublisherWithWriter(failingWriter{}, "t", zap.NewNop())
	if err := p.PublishOutbox(context.Background(), "id", "k", []byte(`{}`)); err == nil {
		t.Fatal("broker failure was swallowed")
	}
}

// The envelope carries schema_version 1.0 and an event_id derived from the
// outbox row id; the message carries that id as X-Event-ID.
func TestNewEvent_SchemaVersionAndStableEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "t", zap.NewNop())
	body, err := json.Marshal(events.NewEvent("row-1", events.PublishParams{EventType: "x.y", ObligationID: "agg-1", TenantID: "t1"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PublishOutbox(context.Background(), "row-1", "agg-1", body); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(w.msgs[0].Value, &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != "1.0" || env["event_id"] != "evt-row-1" || env["event_type"] != "x.y" {
		t.Fatalf("envelope = %v", env)
	}
	if string(w.msgs[0].Key) != "agg-1" || len(w.msgs[0].Headers) != 1 || string(w.msgs[0].Headers[0].Value) != "row-1" {
		t.Fatalf("message key/headers = %q %v", w.msgs[0].Key, w.msgs[0].Headers)
	}
}
