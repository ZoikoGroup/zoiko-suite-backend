// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id) — tenant_id was already
// present. legal_entity_id/jurisdiction are correctly absent: AI governance
// objects here are platform/tenant-scoped, not legal-entity-scoped.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/ai-governance-svc/internal/events"
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

// TestPublish_DecidedEvent_CarriesDeciderNotProposer proves the
// automation_action.decided event attributes the DECIDER, not whoever
// originally proposed the action — self-approval is already blocked at the
// handler level (ErrSelfApprovalBlocked), so these are always two distinct
// principals, and the event's actor_id must reflect which one just acted.
func TestPublish_DecidedEvent_CarriesDeciderNotProposer(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.ai-governance.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "automation_action.decided", EntityID: "action-1", TenantID: "tenant-1",
		ActorID: "decider-1", CorrelationID: "corr-1",
		Payload: map[string]string{"decision": "APPROVED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "automation_action.decided", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "ai-governance-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "decider-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameEntity_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.ai-governance.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "automation_action.proposed", EntityID: "action-1", TenantID: "tenant-1",
			ActorID: "proposer-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
