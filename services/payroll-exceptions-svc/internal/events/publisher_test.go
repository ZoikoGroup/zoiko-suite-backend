// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.PayrollException has no
// legal_entity_id field of its own; the handler resolves one from the
// affected employee, or falls back to a "GLOBAL" sentinel for the authz
// check when there is no employee. That sentinel is not a real legal
// entity, so the handler passes "" instead — the envelope must never
// carry "GLOBAL" as legal_entity_id.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-exceptions-svc/internal/domain"
	"zoiko.io/payroll-exceptions-svc/internal/events"
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

func TestPublishExceptionRaised_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-exceptions.events", w)

	p.PublishExceptionRaised(context.Background(), "corr-1", "entity-1", "raiser-1", domain.PayrollException{
		ExceptionID: "exc-1", TenantID: "tenant-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "payroll.exception.raised", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "payroll-exceptions-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "raiser-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishExceptionRaised_EmptyLegalEntityID_NeverGlobalSentinel(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-exceptions.events", w)

	// Caller passes "" (the handler sanitizes "GLOBAL" to "" before this
	// point) — the envelope must reflect that, never "GLOBAL".
	p.PublishExceptionRaised(context.Background(), "corr-1", "", "raiser-1", domain.PayrollException{ExceptionID: "exc-1"})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Empty(t, env.LegalEntityID)
	assert.NotEqual(t, "GLOBAL", env.LegalEntityID)
}

func TestPublishExceptionResolved_ActorIsResolver(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-exceptions.events", w)

	resolver := "resolver-1"
	p.PublishExceptionResolved(context.Background(), "corr-1", "entity-1", domain.PayrollException{
		ExceptionID: "exc-1", ResolvedBy: &resolver,
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "resolver-1", env.ActorID)
}

func TestPublishBlockerFlagged_RepeatEventsOnSameRun_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-exceptions.events", w)

	for i := 0; i < 2; i++ {
		p.PublishBlockerFlagged(context.Background(), "corr-x", "tenant-1", "entity-1", "flagger-1", "run-1", i+1)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
