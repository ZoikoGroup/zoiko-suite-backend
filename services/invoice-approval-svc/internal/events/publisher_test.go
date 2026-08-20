// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.InvoiceApprovalRequest carries real
// TenantID/LegalEntityID and a CreatedByPrincipalID actor for the started
// event; approved/rejected source their actor from the deciding
// principal, not the original requester. No jurisdiction field exists on
// the domain object.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/invoice-approval-svc/internal/domain"
	"zoiko.io/invoice-approval-svc/internal/events"
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

func TestPublishApproved_ActorIsDecider_NotRequester(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.invoice-approval.events", w)

	p.PublishApproved(context.Background(), "corr-1", "decider-1", domain.InvoiceApprovalRequest{
		ApprovalRequestID: "req-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		CreatedByPrincipalID: "requester-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "invoice.approved", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "invoice-approval-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "decider-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishApprovalStarted_ActorIsCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.invoice-approval.events", w)

	p.PublishApprovalStarted(context.Background(), "corr-1", domain.InvoiceApprovalRequest{
		ApprovalRequestID: "req-1", TenantID: "tenant-1", CreatedByPrincipalID: "requester-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "requester-1", env.ActorID)
}

func TestPublishRejected_RepeatEventsOnSameRequest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.invoice-approval.events", w)

	for i := 0; i < 2; i++ {
		p.PublishRejected(context.Background(), "corr-x", "decider-1", domain.InvoiceApprovalRequest{
			ApprovalRequestID: "req-1", TenantID: "tenant-1",
		}, "insufficient documentation")
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.invoice-approval.events", nil)
	p.PublishApprovalStarted(context.Background(), "corr-1", domain.InvoiceApprovalRequest{ApprovalRequestID: "req-1"})
}
