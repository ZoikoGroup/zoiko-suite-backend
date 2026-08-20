// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.BoardMeeting and
// domain.BoardResolution both carry real LegalEntityID and a
// CreatedBy/PassedBy actor; neither has a jurisdiction field.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/board-resolutions-svc/internal/events"
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
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.board-resolutions.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "resolution.passed", EntityID: "res-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "passer-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "PASSED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "resolution.passed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "board-resolutions-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "passer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameResolution_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.board-resolutions.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "resolution.votes_recorded", EntityID: "res-1", TenantID: "tenant-1",
			ActorID: "voter-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewKafkaPublisher_NoBrokers_DoesNotPanic(t *testing.T) {
	p := events.NewKafkaPublisher(nil, "zoiko.board-resolutions.events", zap.NewNop())
	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "meeting.created", EntityID: "meet-1", TenantID: "tenant-1",
	})
	require.NoError(t, err)
	require.NoError(t, p.Close())
}
