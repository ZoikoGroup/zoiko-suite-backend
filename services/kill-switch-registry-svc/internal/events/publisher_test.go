// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id). tenant_id/legal_entity_id/
// jurisdiction are correctly omitted for a platform-wide switch: kill
// switches are independently-nullable-scoped across plane/domain/
// provider/tenant (doc7 §32.1).
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/kill-switch-registry-svc/internal/events"
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

// TestPublish_PlatformWideSwitch_OmitsTenantHonestly proves a platform-wide
// kill switch (no tenant_id supplied) is not fabricated a tenant.
func TestPublish_PlatformWideSwitch_OmitsTenantHonestly(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.kill-switch-registry.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "kill_switch.engaged", EntityID: "event-1",
		ActorID: "approver-1", CorrelationID: "corr-1",
		Payload: map[string]string{"action": "ENGAGE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "kill_switch.engaged", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "kill-switch-registry-svc", env.SourceService)
	assert.Empty(t, env.TenantID)
	assert.Equal(t, "approver-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_TenantScopedSwitch_CarriesTenant(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.kill-switch-registry.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "kill_switch.disengaged", EntityID: "event-2", TenantID: "tenant-1",
		ActorID: "approver-1", CorrelationID: "corr-2",
		Payload: map[string]string{"action": "DISENGAGE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "tenant-1", env.TenantID)
}

func TestPublish_RepeatEventsOnSameScope_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.kill-switch-registry.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "kill_switch.engaged", EntityID: "event-1",
			ActorID: "approver-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
