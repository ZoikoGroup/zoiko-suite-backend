// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.TaxCalculationRecord carries a
// real JurisdictionCode; legal_entity_id and actor_id are threaded
// through explicitly from the handler (the employee's resolved legal
// entity, and the already-verified principalID).
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-tax-svc/internal/domain"
	"zoiko.io/payroll-tax-svc/internal/events"
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
	Jurisdiction  string `json:"jurisdiction"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublishTaxCalculated_EnvelopeCarriesJurisdictionLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-tax.events", w)

	p.PublishTaxCalculated(context.Background(), "corr-1", "entity-1", "calculator-1", domain.TaxCalculationRecord{
		CalculationID: "calc-1", TenantID: "tenant-1", JurisdictionCode: "uk-england",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "payroll.tax.calculated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "payroll-tax-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "calculator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishTaxAdjusted_RepeatEventsOnSameCalculation_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.payroll-tax.events", w)

	for i := 0; i < 2; i++ {
		p.PublishTaxAdjusted(context.Background(), "corr-x", "entity-1", "adjuster-1", domain.TaxCalculationRecord{
			CalculationID: "calc-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.payroll-tax.events", nil)
	p.PublishTaxException(context.Background(), "corr-1", "tenant-1", "entity-1", "principal-1", "calc-1", "negative taxable basis")
}
