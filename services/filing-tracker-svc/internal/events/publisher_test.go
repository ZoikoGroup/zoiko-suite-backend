// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires, and that repeat events for the same filing no
// longer collide on event_id.
//
// filing.submitted is Doc 03 §3.7's #4 named mandatory case ("filing
// submission") — the last of the six. Same pre-fix state and same real bug
// as contract-lifecycle-svc (6f82b34): a deterministic event_id colliding
// across repeat events on the same filing.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/filing-tracker-svc/internal/events"
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
	Jurisdiction  string `json:"jurisdiction"`
	ActorID       string `json:"actor_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

// TestPublish_EnvelopeCarriesJurisdiction is worth calling out: unlike
// almost every other service in this platform, filing-tracker-svc's own
// domain model has a real jurisdiction field to source this from — so this
// is one of the few events where "jurisdiction context" (Doc 03 §19) is
// actually populated, not correctly-omitted for lack of a source.
func TestPublish_EnvelopeCarriesJurisdiction(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "filing.submitted", FilingID: "filing-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", Jurisdiction: "US-CA", ActorID: "filer-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "SUBMITTED"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "filing.submitted", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "US-CA", env.Jurisdiction)
	assert.Equal(t, "filer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

// TestPublish_RepeatEventsOnSameFiling_GetDistinctEventIDs is the
// regression test for the real bug this fix closes (same class of bug as
// contract-lifecycle-svc's, found independently in this service).
func TestPublish_RepeatEventsOnSameFiling_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "filing.requirement.updated", FilingID: "filing-1", TenantID: "tenant-1",
			ActorID: "editor-1", CorrelationID: "corr-x",
			Payload: map[string]int{"revision": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID,
		"DUPLICATE EVENT_ID: two distinct filing.requirement.updated events on the same filing must not collide")
}
