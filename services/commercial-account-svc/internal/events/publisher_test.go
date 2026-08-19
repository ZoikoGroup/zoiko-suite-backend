// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id). tenant_id is populated from
// OrganizationID, this service's own tenant-scoping concept (doc7's "top-
// level tenant object"). legal_entity_id/jurisdiction are correctly
// omitted — no domain object here carries either.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/events"
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
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.commercial-account.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "commercial_subscription.status_changed", EntityID: "sub-1", TenantID: "org-1",
		ActorID: "operator-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "PAST_DUE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "commercial_subscription.status_changed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "commercial-account-svc", env.SourceService)
	assert.Equal(t, "org-1", env.TenantID)
	assert.Equal(t, "operator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

// TestPublishForOutbox_OmitsActorAndCorrelation_NotFabricated proves the
// outbox-relayed path (which has no actor_id/correlation_id column to draw
// from) genuinely omits them rather than inventing a value — the honest
// consequence of outbox_events' current schema, not a bug in the adapter.
func TestPublishForOutbox_OmitsActorAndCorrelation_NotFabricated(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.commercial-account.events", zap.NewNop())

	err := p.PublishForOutbox(context.Background(), "commercial_subscription.created", "sub-1", "org-1",
		map[string]string{"status": "ACTIVE"})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "commercial_subscription.created", env.EventType)
	assert.Equal(t, "org-1", env.TenantID)
	assert.Empty(t, env.ActorID)
	assert.Empty(t, env.CorrelationID)
}

func TestPublish_RepeatEventsOnSameEntity_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.commercial-account.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "commercial_subscription.plan_changed", EntityID: "sub-1", TenantID: "org-1",
			ActorID: "operator-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
