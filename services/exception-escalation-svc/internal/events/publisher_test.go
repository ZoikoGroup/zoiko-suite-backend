// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.ExceptionCase carries real
// LegalEntityID and JurisdictionID fields.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/events"
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

// TestPublish_ClosedEvent_CarriesResolverNotCreator proves exception.closed
// attributes whoever RESOLVED the case — self-resolution by the original
// creator is already blocked at the handler level.
func TestPublish_ClosedEvent_CarriesResolverNotCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "exception.closed", CaseID: "case-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", Jurisdiction: "jurisdiction-1", ActorID: "resolver-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "CLOSED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "exception.closed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "exception-escalation-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "jurisdiction-1", env.Jurisdiction)
	assert.Equal(t, "resolver-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameCase_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "exception.escalated", CaseID: "case-1", TenantID: "tenant-1",
			ActorID: "escalator-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
