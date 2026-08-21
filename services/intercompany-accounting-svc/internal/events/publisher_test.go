// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.IntercompanyEntry is inherently
// two-entity (source/target legal entity), so the envelope's single
// legal_entity_id slot is correctly left empty rather than arbitrarily
// picking one side — both remain explicit named fields inside the
// payload. actor_id is threaded through explicitly from the handler's
// already-verified principalID.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublishEntryPosted_EnvelopeCarriesTenantAndActor_LegalEntityBothInPayload(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.intercompany-accounting.events", w)

	p.PublishEntryPosted(context.Background(), "corr-1", "principal-1", domain.IntercompanyEntry{
		IntercompanyEntryID: "entry-1", TenantID: "tenant-1",
		SourceLegalEntityID: "entity-source", TargetLegalEntityID: "entity-target",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "intercompany.entry.posted", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "intercompany-accounting-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "principal-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "entity-source", payload["source_legal_entity_id"])
	assert.Equal(t, "entity-target", payload["target_legal_entity_id"])
}

func TestPublishEntryCreated_RepeatEventsOnSameEntry_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.intercompany-accounting.events", w)

	for i := 0; i < 2; i++ {
		p.PublishEntryCreated(context.Background(), "corr-x", "creator-1", domain.IntercompanyEntry{
			IntercompanyEntryID: "entry-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.intercompany-accounting.events", nil)
	p.PublishMismatchDetected(context.Background(), "corr-1", "principal-1", domain.IntercompanyEntry{IntercompanyEntryID: "entry-1"}, "amount mismatch")
}
