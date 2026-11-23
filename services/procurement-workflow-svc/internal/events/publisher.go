// Package events builds this service's events and delivers them from the
// transactional outbox.
//
// procurement.requested, procurement.approval.started and
// procurement.completed are built here as outbox rows and INSERTed by the
// store inside the same transaction as the case change they describe;
// internal/outbox.Relay then calls PublishOutbox, retrying until the broker
// accepts each one. They used to be written straight to Kafka after the
// commit, with the write error logged and dropped.
//
// The envelope keeps the shape consumers already read (Doc 03 §19): event_id,
// event_type, event_version, emitted_at, schema_version "1.0",
// source_service, tenant_id, legal_entity_id, actor_id, correlation_id,
// payload. domain.ProcurementCase carries real TenantID/LegalEntityID and
// RequestedByPrincipalID as the natural actor for the requested /
// approval-started events; procurement.completed takes its actor from the
// principal who issued the purchase order, which may not be the requester. No
// jurisdiction field exists on the domain object.
package events

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/procurement-workflow-svc/internal/domain"
	"zoiko.io/procurement-workflow-svc/internal/outbox"
)

const (
	SourceService        = "procurement-workflow-svc"
	EventRequested       = "procurement.requested"
	EventApprovalStarted = "procurement.approval.started"
	EventCompleted       = "procurement.completed"
	aggregateProcurement = "procurement_case"
)

// Requested fires when a procurement case is first created, whether or not
// the spend check that runs during creation allows it — the event marks the
// request happening, not any particular outcome.
func Requested(c domain.ProcurementCase) outbox.Event {
	return build(EventRequested, c.RequestedByPrincipalID, c, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
		"category":        c.Category,
		"amount":          c.Amount,
		"currency_code":   c.CurrencyCode,
		"status":          c.Status,
	})
}

// ApprovalStarted fires only when the spend check lets the case enter the
// approval stage — a spend-blocked case never reaches this event.
func ApprovalStarted(c domain.ProcurementCase) outbox.Event {
	return build(EventApprovalStarted, c.RequestedByPrincipalID, c, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
	})
}

// Completed fires once the real purchase order has been issued and the case
// reaches its terminal COMPLETED state.
func Completed(actorID string, c domain.ProcurementCase) outbox.Event {
	return build(EventCompleted, actorID, c, map[string]any{
		"case_id":           c.CaseID,
		"legal_entity_id":   c.LegalEntityID,
		"purchase_order_id": c.PurchaseOrderID,
		"amount":            c.Amount,
		"currency_code":     c.CurrencyCode,
	})
}

func build(eventType, actorID string, c domain.ProcurementCase, payload map[string]any) outbox.Event {
	id := uuid.NewString()
	ev := outbox.Event{
		OutboxEventID: id, AggregateType: aggregateProcurement, AggregateID: c.CaseID, EventType: eventType,
		TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, ActorID: actorID, CorrelationID: c.CorrelationID,
		Payload: payload,
	}
	ev.Payload = outbox.NewEnvelope(id, SourceService, eventType, ev, 0)
	return ev
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so tests can assert content without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher delivers outbox rows to Kafka.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the
	// interface itself non-nil, defeating the p.producer == nil check.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishOutbox publishes one already-built envelope, keyed by the case id as
// before. The stable outbox event id travels as the X-Event-ID header across
// every retry so consumers can de-duplicate at-least-once delivery. A failure
// is RETURNED (the relay keeps the row and retries) — it is never swallowed.
// With no producer (dry mode) it logs and reports success, as before.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	if p.producer == nil {
		p.log.Info("simulating publish event in dry mode", zap.String("outbox_event_id", outboxEventID))
		return nil
	}
	err := p.producer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	})
	if err != nil {
		p.log.Warn("outbox kafka write failed",
			zap.String("outbox_event_id", outboxEventID), zap.String("topic", p.topic), zap.Error(err))
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
