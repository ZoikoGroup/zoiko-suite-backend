// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.TaxDetermination carries real
// LegalEntityID and JurisdictionID fields. actor_id is now sourced from
// h.authorize's returned principalID, which the handler previously
// discarded with `_` at both call sites.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tax-determination-svc/internal/events"
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

func TestPublish_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.tax-determination.events", zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "tax_determination.overridden", DeterminationID: "det-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", Jurisdiction: "jurisdiction-1", ActorID: "overrider-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "OVERRIDDEN"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "tax_determination.overridden", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tax-determination-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "jurisdiction-1", env.Jurisdiction)
	assert.Equal(t, "overrider-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameDetermination_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.tax-determination.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "tax_determination.calculated", DeterminationID: "det-1", TenantID: "tenant-1",
			ActorID: "calculator-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
