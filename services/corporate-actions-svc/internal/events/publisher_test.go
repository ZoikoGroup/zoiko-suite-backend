// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, legal_entity_id, actor_id, correlation_id).
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/corporate-actions-svc/internal/events"
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

// TestPublish_ExecutedEvent_CarriesExecutorNotCreator proves
// corporate_action.executed attributes whoever EXECUTED the action — self-
// execution by the original creator is already blocked at the handler
// level (ErrSelfApprovalNotAllowed), so these are always distinct
// principals for a real execution.
func TestPublish_ExecutedEvent_CarriesExecutorNotCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.corporate-actions.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "corporate_action.executed", ActionID: "action-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "executor-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "EXECUTED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "corporate_action.executed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "corporate-actions-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "executor-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameAction_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.corporate-actions.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "corporate_action.updated", ActionID: "action-1", TenantID: "tenant-1",
			ActorID: "editor-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
