// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.VendorDDCheck carries real
// TenantID/LegalEntityID and InitiatedByPrincipalID as its actor — a
// single check runs started→completed/failed within one request, so
// the initiating principal is accurate for all three events. No
// jurisdiction field exists on the domain object.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	"zoiko.io/vendor-due-diligence-svc/internal/events"
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

func TestPublishCompleted_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.vendor-due-diligence.events", w)

	p.PublishCompleted(context.Background(), "corr-1", domain.VendorDDCheck{
		CheckID: "check-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		InitiatedByPrincipalID: "initiator-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "vendor.dd.completed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "vendor-due-diligence-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "initiator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishStarted_RepeatEventsOnSameCheck_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.vendor-due-diligence.events", w)

	for i := 0; i < 2; i++ {
		p.PublishStarted(context.Background(), "corr-x", domain.VendorDDCheck{
			CheckID: "check-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.vendor-due-diligence.events", nil)
	p.PublishFailed(context.Background(), "corr-1", domain.VendorDDCheck{CheckID: "check-1"}, "screening service unavailable")
}
