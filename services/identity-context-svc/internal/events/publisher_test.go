// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires when this service has real data for them.
// Not every event has a tenant/legal-entity/actor to source from (e.g. a
// failed resolution may not have resolved a tenant at all) — those fields
// are correctly omitted per-event rather than fabricated.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
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

func TestPublishContextResolved_EnvelopeCarriesTenantLegalEntityActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.identity-context.events", w)

	err := p.PublishContextResolved(context.Background(), "principal-1", "tenant-1", "entity-1", "session-1", "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "identity.context.resolved", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "identity-context-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "principal-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishResolutionFailed_NoTenantOrLegalEntity_CorrectlyOmitted(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.identity-context.events", w)

	err := p.PublishResolutionFailed(context.Background(), "unknown-subject", "corr-1", "token expired")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.TenantID)
	assert.Empty(t, env.LegalEntityID)
	assert.Empty(t, env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
}

func TestPublishSessionInvalidated_RepeatEventsOnSameSession_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.identity-context.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishSessionInvalidated(context.Background(), "session-1", "principal-1", domain.InvalidationReason("LOGOUT"), "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
