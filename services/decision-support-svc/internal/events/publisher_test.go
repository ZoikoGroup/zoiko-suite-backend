// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires, and that the publisher ships it unchanged.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func sampleRecommendation() domain.Recommendation {
	return domain.Recommendation{
		RecommendationID:       "rec-1",
		TenantID:               "tenant-1",
		LegalEntityID:          "le-1",
		RequestedByPrincipalID: "principal-requester",
		CorrelationID:          "corr-1",
		Status:                 domain.StatusAccepted,
	}
}

func TestNewRecommendationEvent_CarriesDoc03Section19Fields(t *testing.T) {
	msg, err := events.NewRecommendationEvent(events.EventRecommendationAccepted, sampleRecommendation(), "principal-reviewer")
	require.NoError(t, err)

	assert.Equal(t, domain.OutboxDestinationKafka, msg.Destination)
	assert.Equal(t, "rec-1", msg.MessageKey)
	assert.Equal(t, "tenant-1", msg.TenantID)
	assert.NotEmpty(t, msg.OutboxID)

	var env events.Envelope
	require.NoError(t, json.Unmarshal(msg.Payload, &env))
	assert.Contains(t, env.EventID, "evt-")
	assert.Equal(t, events.EventRecommendationAccepted, env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "decision-support-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "le-1", env.LegalEntityID)
	assert.Equal(t, "principal-reviewer", env.ActorID, "the actor is whoever caused this event, not the original requester")
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.False(t, env.EmittedAt.IsZero())

	var payload domain.Recommendation
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, domain.StatusAccepted, payload.Status)
}

func TestNewRecommendationEvent_EventIDsAreUnique(t *testing.T) {
	first, err := events.NewRecommendationEvent(events.EventRecommendationProposed, sampleRecommendation(), "p")
	require.NoError(t, err)
	second, err := events.NewRecommendationEvent(events.EventRecommendationProposed, sampleRecommendation(), "p")
	require.NoError(t, err)

	var a, b events.Envelope
	require.NoError(t, json.Unmarshal(first.Payload, &a))
	require.NoError(t, json.Unmarshal(second.Payload, &b))
	assert.NotEqual(t, a.EventID, b.EventID)
}

func TestEventTypeForStatus(t *testing.T) {
	assert.Equal(t, events.EventRecommendationProposed, events.EventTypeForStatus(domain.StatusReviewRequired))
	assert.Equal(t, events.EventRecommendationAccepted, events.EventTypeForStatus(domain.StatusAccepted))
	assert.Equal(t, events.EventRecommendationRejected, events.EventTypeForStatus(domain.StatusRejected))
	assert.Equal(t, events.EventRecommendationExpired, events.EventTypeForStatus(domain.StatusExpired))
	assert.Equal(t, events.EventRecommendationSuperseded, events.EventTypeForStatus(domain.StatusSuperseded))
}

func TestPublisher_WriteRaw(t *testing.T) {
	writer := &fakeWriter{}
	publisher := events.NewPublisher(writer)

	require.NoError(t, publisher.WriteRaw(context.Background(), "rec-1", []byte(`{"x":1}`)))
	require.Len(t, writer.msgs, 1)
	assert.Equal(t, "rec-1", string(writer.msgs[0].Key))
	assert.JSONEq(t, `{"x":1}`, string(writer.msgs[0].Value))
}

func TestPublisher_DryRunWithoutWriter(t *testing.T) {
	publisher := events.NewPublisher(nil)
	assert.NoError(t, publisher.WriteRaw(context.Background(), "k", []byte("v")))
}
