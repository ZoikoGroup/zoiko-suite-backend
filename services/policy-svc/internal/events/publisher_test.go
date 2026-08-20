// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.Policy is a global definition with
// no tenant/legal-entity scope, correctly omitted. domain.PolicyVersion
// is independently-nullable-scoped — nil TenantID/LegalEntityID (global
// or tenant-wide) is correctly omitted, not fabricated.
// policy.rule.retired carries no actor: it's a system side effect, not
// a principal-driven transition.
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

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/events"
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

func strPtr(s string) *string { return &s }

func TestPublishVersionActivated_EnvelopeCarriesTenantLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.policy.events", w)

	err := p.PublishVersionActivated(context.Background(), domain.PolicyVersion{
		PolicyVersionID: "ver-1", PolicyID: "pol-1",
		TenantID: strPtr("tenant-1"), LegalEntityID: strPtr("entity-1"),
		ActivatedByPrincipalID: strPtr("activator-1"),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "policy.version.activated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "policy-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "activator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishPolicyCreated_NoTenantOrLegalEntity_GlobalDefinition(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.policy.events", w)

	err := p.PublishPolicyCreated(context.Background(), domain.Policy{
		PolicyID: "pol-1", CreatedByPrincipalID: "creator-1", CreatedAt: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.TenantID)
	assert.Empty(t, env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
}

func TestPublishRuleRetired_NoActor_SystemSideEffect(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.policy.events", w)

	err := p.PublishRuleRetired(context.Background(), domain.PolicyVersion{PolicyVersionID: "ver-1", PolicyID: "pol-1"}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.ActorID)
}

func TestPublishPolicyUpdated_RepeatEventsOnSamePolicy_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.policy.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishPolicyUpdated(context.Background(), domain.PolicyVersion{PolicyVersionID: "ver-1", PolicyID: "pol-1"}, "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
