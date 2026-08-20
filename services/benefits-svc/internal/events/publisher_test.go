// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.BenefitElection has no
// legal_entity_id, jurisdiction, or actor field of its own; legal_entity_id
// and actor_id are threaded through explicitly from the handler (the
// enrolled employee's resolved legal entity, and the already-verified
// principalID). jurisdiction stays omitted: no source exists anywhere in
// this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/benefits-svc/internal/domain"
	"zoiko.io/benefits-svc/internal/events"
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

func TestPublishBenefitEnrolled_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.benefits.events", w)

	p.PublishBenefitEnrolled(context.Background(), "corr-1", "entity-1", "principal-1", domain.BenefitElection{
		ElectionID: "elec-1", TenantID: "tenant-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "benefit.enrolled", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "benefits-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "principal-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishBenefitChanged_RepeatEventsOnSameElection_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.benefits.events", w)

	for i := 0; i < 2; i++ {
		p.PublishBenefitChanged(context.Background(), "corr-x", "entity-1", "principal-1", domain.BenefitElection{
			ElectionID: "elec-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.benefits.events", nil)
	p.PublishBenefitTerminated(context.Background(), "corr-1", "entity-1", "principal-1", domain.BenefitElection{ElectionID: "elec-1"})
}
