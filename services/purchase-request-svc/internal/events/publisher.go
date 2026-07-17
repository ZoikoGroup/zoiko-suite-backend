// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event backbone.
// Publish failures are logged, never returned/propagated — same posture as
// every other producer in this platform (an outbox pattern handles
// redelivery; DB writes are never rolled back on a publish failure).
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishRequestCreated corresponds to §12.8's purchase.request.created event.
func (p *Publisher) PublishRequestCreated(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.created", r.CorrelationID, map[string]any{
		"request_id":      r.RequestID,
		"tenant_id":       r.TenantID,
		"legal_entity_id": r.LegalEntityID,
		"amount":          r.Amount,
	})
}

// PublishRequestApproved corresponds to §12.8's purchase.request.approved event.
func (p *Publisher) PublishRequestApproved(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.approved", r.CorrelationID, map[string]any{
		"request_id": r.RequestID,
	})
}

// PublishRequestRejected corresponds to §12.8's purchase.request.rejected event.
func (p *Publisher) PublishRequestRejected(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.rejected", r.CorrelationID, map[string]any{
		"request_id": r.RequestID,
		"reason":     r.RejectionReason,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "purchase-request-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
