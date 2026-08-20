// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.SpendPolicy carries real
// TenantID/LegalEntityID but has no actor field for a submitted spend
// check, so actor_id is threaded through explicitly from the handler's
// already-verified principalID. No jurisdiction field exists on the
// domain object.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	"zoiko.io/spend-controls-svc/internal/events"
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

func TestPublishThresholdBreached_EnvelopeCarriesTenantLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.spend-controls.events", w)

	p.PublishThresholdBreached(context.Background(), "corr-1", "submitter-1",
		domain.SpendCheckRequest{LegalEntityID: "entity-1"},
		domain.SpendPolicy{SpendPolicyID: "policy-1", TenantID: "tenant-1"},
		5000.0)

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "spend.threshold.breached", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "spend-controls-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "submitter-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishBlockApplied_RepeatEventsOnSamePolicy_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.spend-controls.events", w)

	for i := 0; i < 2; i++ {
		p.PublishBlockApplied(context.Background(), "corr-x", "submitter-1",
			domain.SpendCheckRequest{}, domain.SpendPolicy{SpendPolicyID: "policy-1"})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.spend-controls.events", nil)
	p.PublishThresholdBreached(context.Background(), "corr-1", "submitter-1", domain.SpendCheckRequest{}, domain.SpendPolicy{}, 0)
}
