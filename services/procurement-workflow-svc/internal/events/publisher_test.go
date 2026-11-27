// Package events_test asserts the event envelope carries the fields Doc 03
// §19 requires now that the events are built for the outbox. The requester is
// the actor of requested/approval-started; procurement.completed takes its
// actor from the order-issuing principal.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/procurement-workflow-svc/internal/domain"
	"zoiko.io/procurement-workflow-svc/internal/events"
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
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	EventVersion  string         `json:"event_version"`
	SchemaVersion string         `json:"schema_version"`
	SourceService string         `json:"source_service"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	ActorID       string         `json:"actor_id"`
	CorrelationID string         `json:"correlation_id"`
	Payload       map[string]any `json:"payload"`
}

func decode(t *testing.T, payload any) envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var env envelope
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

var sample = domain.ProcurementCase{
	CaseID: "case-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
	RequestedByPrincipalID: "requester-1", CorrelationID: "corr-1", Amount: 42, CurrencyCode: "GBP",
	Category: "IT", Status: domain.CaseStatusApprovalPending,
}

func TestRequested_ActorIsRequester_EnvelopeComplete(t *testing.T) {
	ev := events.Requested(sample)
	assert.Equal(t, "procurement.requested", ev.EventType)
	assert.Equal(t, "case-1", ev.AggregateID)
	env := decode(t, ev.Payload)
	assert.Equal(t, "procurement.requested", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "procurement-workflow-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.Equal(t, "evt-"+ev.OutboxEventID, env.EventID)
	assert.Equal(t, "case-1", env.Payload["case_id"])
	assert.Equal(t, 42.0, env.Payload["amount"])
	assert.Equal(t, "APPROVAL_PENDING", env.Payload["status"])
}

func TestCompleted_ActorIsOrderIssuer_NotOriginalRequester(t *testing.T) {
	c := sample
	po := "po-1"
	c.PurchaseOrderID = &po
	env := decode(t, events.Completed("issuer-1", c).Payload)
	assert.Equal(t, "procurement.completed", env.EventType)
	assert.Equal(t, "issuer-1", env.ActorID)
	assert.Equal(t, "po-1", env.Payload["purchase_order_id"])
}

func TestApprovalStarted_RepeatEventsOnSameCase_GetDistinctEventIDs(t *testing.T) {
	a, b := events.ApprovalStarted(sample), events.ApprovalStarted(sample)
	assert.NotEqual(t, decode(t, a.Payload).EventID, decode(t, b.Payload).EventID)
	assert.Equal(t, "procurement.approval.started", decode(t, a.Payload).EventType)
}

func TestPublishOutbox_KeyHeader_ErrorReturned_DryMode(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.procurement-workflow.events", w)
	require.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "case-1", []byte(`{}`)))
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "case-1", string(w.msgs[0].Key))
	assert.Equal(t, "ob-1", string(w.msgs[0].Headers[0].Value))

	w.err = errors.New("broker down")
	assert.Error(t, p.PublishOutbox(context.Background(), "ob-2", "case-1", []byte(`{}`)))

	assert.NoError(t, events.NewPublisher(zap.NewNop(), "t", nil).PublishOutbox(context.Background(), "ob-3", "case-1", []byte(`{}`)))
}
