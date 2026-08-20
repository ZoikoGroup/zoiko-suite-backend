// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.Recommendation carries real
// TenantID/LegalEntityID and RequestedByPrincipalID as its actor; it has
// no jurisdiction field, correctly omitted.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/events"
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
	TenantID      string `json:"tenant_id"`
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

func TestPublishRecommended_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.decision-support.events", w)

	p.PublishRecommended(context.Background(), domain.Recommendation{
		RecommendationID: "rec-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		RequestedByPrincipalID: "requester-1", CorrelationID: "corr-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "decision_support.recommended", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "decision-support-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishRecommended_RepeatEventsOnSameRecommendation_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.decision-support.events", w)

	for i := 0; i < 2; i++ {
		p.PublishRecommended(context.Background(), domain.Recommendation{
			RecommendationID: "rec-1", TenantID: "tenant-1", CorrelationID: "corr-x",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.decision-support.events", nil)
	p.PublishRecommended(context.Background(), domain.Recommendation{RecommendationID: "rec-1"})
}
