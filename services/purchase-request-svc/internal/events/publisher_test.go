// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.PurchaseRequest carries real
// TenantID/LegalEntityID and per-lifecycle-stage actors
// (RequestedBy/ApprovedBy/RejectedByPrincipalID). No jurisdiction field
// exists on the domain object.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	"zoiko.io/purchase-request-svc/internal/events"
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

func principalPtr(id string) *string { return &id }

func TestPublishRequestApproved_ActorIsApprover(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-request.events", w)

	p.PublishRequestApproved(context.Background(), domain.PurchaseRequest{
		RequestID: "req-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		RequestedByPrincipalID: "requester-1", ApprovedByPrincipalID: principalPtr("approver-1"),
		CorrelationID: "corr-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "purchase.request.approved", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "purchase-request-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "approver-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishRequestRejected_NilActorPointer_DoesNotPanic(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-request.events", w)

	p.PublishRequestRejected(context.Background(), domain.PurchaseRequest{RequestID: "req-1"})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Empty(t, env.ActorID)
}

func TestPublishRequestCreated_RepeatEventsOnSameRequest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-request.events", w)

	for i := 0; i < 2; i++ {
		p.PublishRequestCreated(context.Background(), domain.PurchaseRequest{
			RequestID: "req-1", TenantID: "tenant-1", CorrelationID: "corr-x",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
