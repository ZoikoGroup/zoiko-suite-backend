// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.VATReturn carries real
// LegalEntityID and JurisdictionID fields. actor_id is now sourced from
// authorize's returned principalID, which the handler previously
// discarded entirely (authorize only returned a bool).
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

	"zoiko.io/vat-gst-svc/internal/events"
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
	Jurisdiction  string `json:"jurisdiction"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.vat-gst.events", zap.NewNop())

	err := publishVia(p, events.PublishParams{
		EventType: "vat_return.filed", ReturnID: "vret-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", Jurisdiction: "uk-england", ActorID: "filer-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "FILED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "vat_return.filed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "vat-gst-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "filer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameReturn_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.vat-gst.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := publishVia(p, events.PublishParams{
			EventType: "vat_return.updated", ReturnID: "vret-1", TenantID: "tenant-1",
			ActorID: "updater-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

// publishVia exercises the delivery path the outbox uses: the envelope built
// by NewEvent at transaction time, delivered by PublishOutbox.
func publishVia(p *events.KafkaPublisher, params events.PublishParams) error {
	data, err := json.Marshal(events.NewEvent(params))
	if err != nil {
		return err
	}
	return p.PublishOutbox(context.Background(), "ob-test", "agg-test", data)
}

func TestNewEvent_CarriesSchemaVersion(t *testing.T) {
	assert.Equal(t, "1.0", events.NewEvent(events.PublishParams{EventType: "x"}).SchemaVersion)
}

func TestPublishOutbox_KeysByAggregateAndCarriesStableEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "topic", zap.NewNop())

	require.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "agg-1", []byte(`{"event_type":"x"}`)))
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "agg-1", string(w.msgs[0].Key))
	assert.JSONEq(t, `{"event_type":"x"}`, string(w.msgs[0].Value))
	require.Len(t, w.msgs[0].Headers, 1)
	assert.Equal(t, "X-Event-ID", w.msgs[0].Headers[0].Key)
	assert.Equal(t, "ob-1", string(w.msgs[0].Headers[0].Value))
}

type failingWriter struct{}

func (failingWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return errors.New("broker down")
}

// The former Publish logged and returned nil on a broker failure; the outbox
// relay must see the error so it keeps the row and retries.
func TestPublishOutbox_ReturnsBrokerError(t *testing.T) {
	p := events.NewKafkaPublisherWithWriter(failingWriter{}, "topic", zap.NewNop())
	require.Error(t, p.PublishOutbox(context.Background(), "ob-1", "agg-1", []byte(`{}`)))
}
