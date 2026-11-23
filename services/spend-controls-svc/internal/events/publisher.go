// Package events builds this service's events and delivers them from the
// transactional outbox.
//
// spend.threshold.breached and spend.block.applied are built here as outbox
// rows (BlockedEvents) and INSERTed by the store inside the same transaction
// that records the BLOCKED consumption; internal/outbox.Relay then calls
// PublishOutbox, retrying until the broker accepts each one. They used to be
// written straight to Kafka after the commit with the error logged and
// dropped, so a broker hiccup lost the only notice of a refused spend.
//
// The envelope keeps the shape consumers already read (Doc 03 §19): event_id,
// event_type, event_version, emitted_at, schema_version "1.0",
// source_service, tenant_id, legal_entity_id, actor_id, correlation_id,
// payload. domain.SpendPolicy carries real TenantID/LegalEntityID but no actor
// for a submitted spend check, so actor_id is the handler's verified principal.
package events

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	"zoiko.io/spend-controls-svc/internal/outbox"
)

const (
	SourceService          = "spend-controls-svc"
	EventThresholdBreached = "spend.threshold.breached"
	EventBlockApplied      = "spend.block.applied"
)

// BlockedEvents are the two outbox rows a fresh BLOCKED decision emits, in
// order. Each has a fresh outbox id (event_id = "evt-" + that id), the policy
// as its aggregate (the Kafka key, as before) and the envelope as its payload.
func BlockedEvents(tenantID, correlationID string, in domain.SpendEvaluation, policy domain.SpendPolicy, projected float64) []outbox.Event {
	breached := map[string]any{
		"spend_policy_id":  policy.SpendPolicyID,
		"legal_entity_id":  in.LegalEntityID,
		"category":         in.Category,
		"attempted_amount": in.Amount,
		"projected_total":  projected,
		"threshold_amount": policy.ThresholdAmount,
		"currency_code":    in.CurrencyCode,
	}
	applied := map[string]any{
		"spend_policy_id":  policy.SpendPolicyID,
		"legal_entity_id":  in.LegalEntityID,
		"category":         in.Category,
		"source_reference": in.SourceReference,
		"blocked_at":       time.Now().UTC(),
	}
	return []outbox.Event{
		build(EventThresholdBreached, tenantID, correlationID, in, policy, breached),
		build(EventBlockApplied, tenantID, correlationID, in, policy, applied),
	}
}

func build(eventType, tenantID, correlationID string, in domain.SpendEvaluation, policy domain.SpendPolicy, payload map[string]any) outbox.Event {
	id := uuid.NewString()
	ev := outbox.Event{
		OutboxEventID: id, AggregateType: "spend_policy", AggregateID: policy.SpendPolicyID, EventType: eventType,
		TenantID: tenantID, LegalEntityID: in.LegalEntityID, ActorID: in.PrincipalID, CorrelationID: correlationID,
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

// PublishOutbox publishes one already-built envelope. The stable outbox event
// id travels as the X-Event-ID header across every retry so consumers can
// de-duplicate at-least-once delivery. A failure is RETURNED (the relay keeps
// the row and retries) — it is never swallowed.
//
// Dry mode (no producer; config permits it only for ENV=local) logs and
// reports success, as the direct publish did.
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
