// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.CashBalance has no legal_entity_id
// field of its own — threaded through explicitly from the handler (the
// bank account's legal entity). domain.EffectiveCashResponse already
// carries real TenantID/LegalEntityID. actor_id is threaded through
// explicitly from the handler's already-verified principalID in both
// cases. No jurisdiction field exists anywhere in this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/events"
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

func TestPublishCashPositionUpdated_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.treasury.events", w)

	p.PublishCashPositionUpdated(context.Background(), "corr-1", "entity-1", "transferrer-1", domain.CashBalance{
		BalanceID: "bal-1", TenantID: "tenant-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "cash.position.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "treasury-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "transferrer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishLiquidityThresholdBreached_RepeatEventsOnSameEntity_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.treasury.events", w)

	for i := 0; i < 2; i++ {
		p.PublishLiquidityThresholdBreached(context.Background(), "corr-x", "viewer-1", domain.EffectiveCashResponse{
			TenantID: "tenant-1", LegalEntityID: "entity-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
