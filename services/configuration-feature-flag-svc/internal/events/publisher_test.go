// Package events_test asserts the event envelope actually carries the fields
// Doc 03 §19 requires that this service has real data for (event_version,
// actor_id, correlation_id). tenant_id is nil-safe: domain.ConfigEntry and
// domain.FeatureFlag are independently-nullable-scoped — nil TenantID (a global
// default) is correctly omitted, not fabricated. legal_entity_id and
// jurisdiction are correctly omitted entirely: config and feature flags are
// environment/tenant-scoped, not legal-entity or jurisdiction-scoped.
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
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func decode(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env
}

func payloadField(t *testing.T, env envelope, field string) any {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &p))
	return p[field]
}

func tenantPtr(id string) *string { return &id }

func TestConfigUpdated_EnvelopeCarriesTenantAndActor(t *testing.T) {
	out, err := events.ConfigUpdated(domain.ConfigEntry{
		ConfigID: "cfg-1", Key: "payroll.batch_size",
		TenantID: tenantPtr("tenant-1"), CreatedByPrincipalID: "creator-1",
		EffectiveFrom: time.Now(),
	}, "corr-1")
	require.NoError(t, err)

	env := decode(t, out.Body)
	assert.Equal(t, "config.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "configuration-feature-flag-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

// The partition key is the aggregate id, not the correlation id. Keying on the
// correlation id put two changes to the SAME config entry on different
// partitions whenever they arrived on different requests, so a consumer
// replaying them could apply an older value after a newer one and serve the
// superseded value permanently.
func TestConfigUpdated_PartitionKeyIsTheAggregateNotTheCorrelation(t *testing.T) {
	out, err := events.ConfigUpdated(domain.ConfigEntry{
		ConfigID: "cfg-1", EffectiveFrom: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	assert.Equal(t, "cfg-1", out.Key)
}

func TestFeatureFlagUpdated_NilTenantID_OmittedNotFabricated(t *testing.T) {
	out, err := events.FeatureFlagUpdated(domain.FeatureFlag{
		FlagID: "flag-1", TenantID: nil, CreatedByPrincipalID: "creator-1",
		EffectiveFrom: time.Now(),
	}, "corr-1")
	require.NoError(t, err)

	env := decode(t, out.Body)
	assert.Empty(t, env.TenantID)
}

// A consumer must be able to tell "this changed for one organisation" from
// "the default every organisation without its own value reads has changed".
// Inferring it from a null tenant_id is exactly the step a consumer gets wrong
// — a missing tenant_id reads as "unknown", not as "all".
func TestBuilders_GlobalScopeIsStatedNotInferred(t *testing.T) {
	global, err := events.FeatureFlagUpdated(domain.FeatureFlag{FlagID: "f", TenantID: nil}, "c")
	require.NoError(t, err)
	assert.Equal(t, true, payloadField(t, decode(t, global.Body), "scope_is_global"))

	scoped, err := events.ConfigUpdated(domain.ConfigEntry{ConfigID: "c", TenantID: tenantPtr("t1")}, "c")
	require.NoError(t, err)
	assert.Equal(t, false, payloadField(t, decode(t, scoped.Body), "scope_is_global"))
}

func TestBuild_RepeatEventsOnSameAggregate_GetDistinctEventIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		out, err := events.ConfigUpdated(domain.ConfigEntry{ConfigID: "cfg-1"}, "corr-x")
		require.NoError(t, err)
		id := decode(t, out.Body).EventID
		assert.False(t, seen[id], "event_id reused across two publishes")
		seen[id] = true
	}
}

func TestPublish_WritesTheWholeBatchInOneCall(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration.events", w)

	require.NoError(t, p.Publish(context.Background(), []kafka.Message{
		{Key: []byte("a"), Value: []byte(`{"n":1}`)},
		{Key: []byte("b"), Value: []byte(`{"n":2}`)},
	}))
	require.Len(t, w.msgs, 2)
}

func TestPublish_EmptyBatchIsANoOp(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration.events", w)
	require.NoError(t, p.Publish(context.Background(), nil))
	assert.Empty(t, w.msgs)
}

// A publish failure must be REPORTED, not swallowed. The relay marks the batch
// for retry on this error; a nil return would mark the events published and
// lose them.
func TestPublish_WriteFailureIsReturned(t *testing.T) {
	w := &fakeWriter{err: assert.AnError}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.configuration.events", w)
	err := p.Publish(context.Background(), []kafka.Message{{Value: []byte("{}")}})
	require.Error(t, err)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.configuration.events", nil)
	require.NoError(t, p.Publish(context.Background(), []kafka.Message{{Value: []byte("{}")}}))
}
