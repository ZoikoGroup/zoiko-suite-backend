// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, actor_id, correlation_id). tenant_id is nil-safe:
// domain.ConfigEntry/FeatureFlag are independently-nullable-scoped —
// nil TenantID (a global default) is correctly omitted, not fabricated.
// legal_entity_id and jurisdiction are correctly omitted entirely: config
// and feature flags are environment/tenant-scoped, not legal-entity or
// jurisdiction-scoped.
package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/events"
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

func tenantPtr(id string) *string { return &id }

func TestPublishConfigUpdated_EnvelopeCarriesTenantAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration-feature-flag.events", w)

	err := p.PublishConfigUpdated(context.Background(), domain.ConfigEntry{
		ConfigID: "cfg-1", TenantID: tenantPtr("tenant-1"), CreatedByPrincipalID: "creator-1",
		EffectiveFrom: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "config.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "configuration-feature-flag-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishFeatureFlagUpdated_NilTenantID_OmittedNotFabricated(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration-feature-flag.events", w)

	err := p.PublishFeatureFlagUpdated(context.Background(), domain.FeatureFlag{
		FlagID: "flag-1", TenantID: nil, CreatedByPrincipalID: "creator-1",
		EffectiveFrom: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.TenantID)
}

func TestPublishConfigUpdated_RepeatEventsOnSameConfig_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration-feature-flag.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishConfigUpdated(context.Background(), domain.ConfigEntry{
			ConfigID: "cfg-1", EffectiveFrom: time.Now(),
		}, "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.configuration-feature-flag.events", nil)
	err := p.PublishFeatureFlagUpdated(context.Background(), domain.FeatureFlag{FlagID: "flag-1"}, "corr-1")
	require.NoError(t, err)
}
