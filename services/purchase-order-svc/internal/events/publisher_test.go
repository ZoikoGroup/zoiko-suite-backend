// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.PurchaseOrder carries real
// TenantID/LegalEntityID and IssuedBy/ClosedByPrincipalID actors;
// amended has no actor field on the order itself, so actor_id is
// threaded through explicitly from the handler's already-verified
// principalID. No jurisdiction field exists on the domain object.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/events"
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

func closerPtr(id string) *string { return &id }

func TestPublishOrderClosed_ActorIsCloser(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-order.events", w)

	p.PublishOrderClosed(context.Background(), domain.PurchaseOrder{
		PurchaseOrderID: "po-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		ClosedByPrincipalID: closerPtr("closer-1"),
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "purchase.order.closed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "purchase-order-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "closer-1", env.ActorID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishOrderAmended_ActorThreadedExplicitly(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-order.events", w)

	p.PublishOrderAmended(context.Background(), "amender-1", domain.PurchaseOrder{
		PurchaseOrderID: "po-1", TenantID: "tenant-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "amender-1", env.ActorID)
}

func TestPublishOrderIssued_RepeatEventsOnSameOrder_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-order.events", w)

	for i := 0; i < 2; i++ {
		p.PublishOrderIssued(context.Background(), domain.PurchaseOrder{
			PurchaseOrderID: "po-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
