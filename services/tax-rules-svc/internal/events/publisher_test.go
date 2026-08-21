// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, jurisdiction, actor_id, correlation_id).
// domain.TaxRule carries a real JurisdictionID but no legal_entity_id
// field.
package events_test

import (
	"context"
	"encoding/json"
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

func TestPublish_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.tax-rules.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "tax_rule.updated", RuleID: "trule-1", TenantID: "tenant-1",
		Jurisdiction: "uk-england", ActorID: "updater-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "ACTIVE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "tax_rule.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tax-rules-svc", env.SourceService)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "updater-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameRule_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.tax-rules.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "tax_rule.created", RuleID: "trule-1", TenantID: "tenant-1",
			ActorID: "creator-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
