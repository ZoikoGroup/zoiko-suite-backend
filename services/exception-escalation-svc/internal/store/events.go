package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/outbox"
)

// Event types emitted by this service's writes. Topic, type and payload are
// unchanged from the former direct publish in the handlers; the event is now
// written to the transactional outbox inside the write's own transaction, so
// it exists if and only if the change committed (internal/outbox.Relay
// delivers it with retry).
const (
	EventExceptionCreated   = "exception.created"
	EventExceptionEscalated = "exception.escalated"
	EventExceptionClosed    = "exception.closed"
	EventCaseClosed         = "case.closed"
	EventTaskCreated        = "task.created"
	EventTaskAssigned       = "task.assigned"
	EventTaskEscalated      = "task.escalated"
	EventTaskCompleted      = "task.completed"
	EventTaskReopened       = "task.reopened"
	EventDeadlineCreated    = "deadline.created"
	EventDeadlineSuperseded = "deadline.superseded"
	EventDeadlineCompleted  = "deadline.completed"
	EventDeadlineDueSoon    = "deadline.due_soon"
	EventDeadlineOverdue    = "deadline.overdue"
)

// EventMeta is the request identity recorded on the events a write emits:
// the acting principal (gateway-verified X-Principal-Id) and correlation ID.
type EventMeta struct {
	ActorID       string
	CorrelationID string
}

type eventMetaKey struct{}

// WithEventMeta attaches the request identity the store stamps on outbox
// events. The HTTP layer sets it once per request (handler.withEventMeta) so
// the store interfaces do not change shape.
func WithEventMeta(ctx context.Context, m EventMeta) context.Context {
	return context.WithValue(ctx, eventMetaKey{}, m)
}

// EventMetaFrom returns the request identity attached by WithEventMeta.
func EventMetaFrom(ctx context.Context) EventMeta {
	m, _ := ctx.Value(eventMetaKey{}).(EventMeta)
	return m
}

// eventSubject identifies what an event is about.
type eventSubject struct {
	aggregateType string
	id            string // envelope exception_case_id and Kafka key, as before
	tenantID      string
	legalEntityID string
	jurisdiction  string
}

// enqueue writes one event to the outbox using the caller's transaction.
func enqueue(ctx context.Context, tx pgx.Tx, eventType string, sub eventSubject, payload any) error {
	meta := EventMetaFrom(ctx)
	id := uuid.NewString()
	envelope := events.BuildEvent(events.PublishParams{
		EventType: eventType, CaseID: sub.id, TenantID: sub.tenantID,
		LegalEntityID: sub.legalEntityID, Jurisdiction: sub.jurisdiction,
		ActorID: meta.ActorID, CorrelationID: meta.CorrelationID, Payload: payload,
	}, "evt-"+id, time.Now())
	return outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: id, AggregateType: sub.aggregateType, AggregateID: sub.id,
		EventType: eventType, TenantID: sub.tenantID, LegalEntityID: sub.legalEntityID,
		ActorID: meta.ActorID, CorrelationID: meta.CorrelationID, Payload: envelope,
	})
}
