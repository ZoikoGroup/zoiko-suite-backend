// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, legal_entity_id, actor_id, correlation_id). tenant_id
// is correctly omitted: RecordAccessDecision never persists a tenant
// scope on domain.AccessDecisionLog.
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

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
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
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublishAuthorizationGranted_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	err := p.PublishAuthorizationGranted(context.Background(), domain.AccessDecisionLog{
		AccessDecisionID: "dec-1", PrincipalID: "principal-1", LegalEntityID: "entity-1",
		ActionType: "PAYMENT_INITIATE", DecisionOutcome: "GRANTED", DecisionBasis: "rbac:role=FINANCE_APPROVER",
		CorrelationID: "corr-1", DecidedAt: time.Now(),
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "authorization.granted", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "authorization-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "principal-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishAuthorizationDenied_RepeatEventsOnSameDecision_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishAuthorizationDenied(context.Background(), domain.AccessDecisionLog{
			AccessDecisionID: "dec-1", PrincipalID: "principal-1", LegalEntityID: "entity-1",
			ActionType: "PAYMENT_INITIATE", DecisionOutcome: "DENIED", DecisionBasis: "no_grant",
			CorrelationID: "corr-x", DecidedAt: time.Now(),
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
