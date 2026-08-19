// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires, and that repeat events for the same contract
// no longer collide on event_id.
//
// Contract execution is Doc 03 §3.7's #5 named mandatory case. Before this
// fix: no actor, no correlation_id, no event_version anywhere on any of the
// five contract events, and — more seriously — event_id was the
// deterministic string "evt-"+eventType+"-"+contractID, which is IDENTICAL
// across every repeat occurrence of the same event type on the same
// contract (e.g. two contract.updated events on the same contract produce
// the same event_id). Any consumer deduplicating via
// INSERT ... ON CONFLICT (event_id) DO NOTHING — the exact pattern this
// platform's own evidence/history consumers use — would silently drop the
// second, real event as if it were a redelivery of the first.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/contract-lifecycle-svc/internal/events"
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
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesActorAndCorrelationID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.contract.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "contract.activated", ContractID: "contract-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "signer-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "ACTIVE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "contract.activated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "signer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

// TestPublish_RepeatEventsOnSameContract_GetDistinctEventIDs is the
// regression test for the real bug this fix closes.
func TestPublish_RepeatEventsOnSameContract_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.contract.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "contract.updated", ContractID: "contract-1", TenantID: "tenant-1",
			LegalEntityID: "entity-1", ActorID: "editor-1", CorrelationID: "corr-x",
			Payload: map[string]int{"version": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID,
		"DUPLICATE EVENT_ID: two distinct contract.updated events on the same contract must not collide — a dedup consumer would drop the second one")
}
