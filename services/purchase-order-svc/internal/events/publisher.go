// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
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

// PublishOrderIssued corresponds to §12.9's purchase.order.issued event.
func (p *Publisher) PublishOrderIssued(ctx context.Context, o domain.PurchaseOrder) {
	p.emit(ctx, "purchase.order.issued", o.CorrelationID, map[string]any{
		"purchase_order_id":   o.PurchaseOrderID,
		"tenant_id":           o.TenantID,
		"legal_entity_id":     o.LegalEntityID,
		"purchase_request_id": o.PurchaseRequestID,
		"po_number":           o.PONumber,
		"total_amount":        o.TotalAmount,
		"currency_code":       o.CurrencyCode,
	})
}

// PublishOrderAmended corresponds to §12.9's purchase.order.amended event.
func (p *Publisher) PublishOrderAmended(ctx context.Context, o domain.PurchaseOrder) {
	p.emit(ctx, "purchase.order.amended", o.CorrelationID, map[string]any{
		"purchase_order_id": o.PurchaseOrderID,
		"version":           o.Version,
		"total_amount":      o.TotalAmount,
	})
}

// PublishOrderClosed corresponds to §12.9's purchase.order.closed event.
func (p *Publisher) PublishOrderClosed(ctx context.Context, o domain.PurchaseOrder) {
	p.emit(ctx, "purchase.order.closed", o.CorrelationID, map[string]any{
		"purchase_order_id": o.PurchaseOrderID,
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
		SourceService: "purchase-order-svc",
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
