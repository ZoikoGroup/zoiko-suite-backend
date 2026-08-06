package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/procurement-workflow-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishRequested fires when a procurement case is first created, whether
// or not the spend-check that runs synchronously during creation allows it
// — the event marks the request happening, not any particular outcome.
func (p *Publisher) PublishRequested(ctx context.Context, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.requested", c.CorrelationID, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
		"category":        c.Category,
		"amount":          c.Amount,
		"currency_code":   c.CurrencyCode,
		"status":          c.Status,
	})
}

// PublishApprovalStarted fires only when the spend-check allows the case to
// enter the approval stage — a spend-blocked case never reaches this event.
func (p *Publisher) PublishApprovalStarted(ctx context.Context, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.approval.started", c.CorrelationID, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
	})
}

// PublishCompleted fires once the real purchase order has been issued and
// the case reaches its terminal COMPLETED state.
func (p *Publisher) PublishCompleted(ctx context.Context, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.completed", c.CorrelationID, map[string]any{
		"case_id":           c.CaseID,
		"legal_entity_id":   c.LegalEntityID,
		"purchase_order_id": c.PurchaseOrderID,
		"amount":            c.Amount,
		"currency_code":     c.CurrencyCode,
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
		SourceService: "procurement-workflow-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if p.producer == nil {
		p.log.Info("simulating publish event in dry mode", zap.String("event_type", eventType))
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
