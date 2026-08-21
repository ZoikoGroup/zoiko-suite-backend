// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires (event_version, tenant_id, legal_entity_id,
// actor_id). domain.PayrollRun has no actor field of its own — actor_id is
// sourced from the already-authorized principal at the calling handler,
// threaded through as an explicit parameter, not fabricated.
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

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decodeOne(t *testing.T, w *fakeWriter) envelope {
	t.Helper()
	require.Len(t, w.msgs, 1)
	var env envelope
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	return env
}

func run() domain.PayrollRun {
	return domain.PayrollRun{
		RunID:         "run-1",
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		RunNumber:     "2026-08",
		CorrelationID: "corr-1",
		CreatedAt:     time.Now().UTC(),
	}
}

// TestPublishRunCompleted_EnvelopeCarriesFinalizingActor is the important
// one: payroll execution is doc §3.7's #2 named mandatory case, and before
// this fix the envelope carried no actor at all for any of the four
// lifecycle events — the audit trail for who actually finalized a payroll
// run (real money to real employees) lived nowhere on the event itself.
func TestPublishRunCompleted_EnvelopeCarriesFinalizingActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.payroll-run.events", w)

	p.PublishRunCompleted(context.Background(), "corr-1", "finalizer-1", run())

	env := decodeOne(t, w)
	assert.Equal(t, "payroll.run.completed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "finalizer-1", env.ActorID)
}

func TestPublishRunInitiated_EnvelopeCarriesInitiator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.payroll-run.events", w)

	p.PublishRunInitiated(context.Background(), "corr-1", "initiator-1", run())

	env := decodeOne(t, w)
	assert.Equal(t, "initiator-1", env.ActorID)
}

func TestPublishRunBlocked_EnvelopeCarriesActorWhoTriggeredCalculation(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.payroll-run.events", w)

	p.PublishRunBlocked(context.Background(), "corr-1", "calculator-1", run(), "contract lookup failed")

	env := decodeOne(t, w)
	assert.Equal(t, "payroll.run.blocked", env.EventType)
	assert.Equal(t, "calculator-1", env.ActorID)
}

// TestPublishRunCompleted_NilProducer_DryModeDoesNotPanic proves the
// existing "dry mode" (no Kafka configured) still works after narrowing
// producer to an interface.
func TestPublishRunCompleted_NilProducer_DryModeDoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.payroll-run.events", nil)

	assert.NotPanics(t, func() {
		p.PublishRunCompleted(context.Background(), "corr-1", "finalizer-1", run())
	})
}
