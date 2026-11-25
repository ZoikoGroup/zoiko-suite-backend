// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, jurisdiction, actor_id, correlation_id).
// domain.TaxRule carries a real JurisdictionID but no legal_entity_id
// field.
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

	"zoiko.io/tax-rules-svc/internal/events"
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

func TestNewEvent_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	evt := events.NewEvent(events.PublishParams{
		EventType: "tax_rule.updated", RuleID: "trule-1", TenantID: "tenant-1",
		Jurisdiction: "uk-england", ActorID: "updater-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "ACTIVE"},
	})
	data, err := json.Marshal(evt)
	require.NoError(t, err)

	env := decode(t, kafka.Message{Value: data})
	assert.Equal(t, "tax_rule.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", evt.SchemaVersion)
	assert.Equal(t, "tax-rules-svc", env.SourceService)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "updater-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestNewEvent_RepeatEventsOnSameRule_GetDistinctEventIDs(t *testing.T) {
	p := events.PublishParams{EventType: "tax_rule.created", RuleID: "trule-1", TenantID: "tenant-1"}
	assert.NotEqual(t, events.NewEvent(p).EventID, events.NewEvent(p).EventID)
}

func TestPublishOutbox_KeysByAggregateAndCarriesStableEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.tax-rules.events", zap.NewNop())

	require.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "trule-1", []byte(`{"event_type":"tax_rule.created"}`)))
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "trule-1", string(w.msgs[0].Key))
	assert.JSONEq(t, `{"event_type":"tax_rule.created"}`, string(w.msgs[0].Value))
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
	p := events.NewKafkaPublisherWithWriter(failingWriter{}, "zoiko.tax-rules.events", zap.NewNop())
	require.Error(t, p.PublishOutbox(context.Background(), "ob-1", "trule-1", []byte(`{}`)))
}
