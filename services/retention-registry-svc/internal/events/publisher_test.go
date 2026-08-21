// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id). tenant_id/jurisdiction are
// genuinely nullable — a platform-wide retention policy/legal hold has
// neither, and this test proves that's honestly represented, not
// fabricated.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/retention-registry-svc/internal/events"
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

func TestPublish_TenantScopedPolicy_CarriesTenantAndJurisdiction(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.retention-registry.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "retention_policy.created", EntityID: "policy-1", TenantID: "tenant-1",
		Jurisdiction: "US-CA", ActorID: "creator-1", CorrelationID: "corr-1",
		Payload: map[string]string{"record_class": "PAYROLL"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "retention_policy.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "retention-registry-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "US-CA", env.Jurisdiction)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_PlatformWidePolicy_OmitsTenantAndJurisdictionHonestly(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.retention-registry.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "legal_hold.engaged", EntityID: "hold-1",
		ActorID: "creator-1", CorrelationID: "corr-2",
		Payload: map[string]string{"scope": "platform-wide"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.TenantID)
	assert.Empty(t, env.Jurisdiction)
}

func TestPublish_RepeatEventsOnSameEntity_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.retention-registry.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "legal_hold.released", EntityID: "hold-1",
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
