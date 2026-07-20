package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
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

// PublishInvoiceIssued publishes invoice.issued event.
func (p *Publisher) PublishInvoiceIssued(ctx context.Context, inv domain.CustomerInvoice) {
	p.emit(ctx, "invoice.issued", inv.CorrelationID, map[string]any{
		"invoice_id":      inv.InvoiceID,
		"tenant_id":       inv.TenantID,
		"legal_entity_id": inv.LegalEntityID,
		"customer_id":     inv.CustomerID,
	})
}

// PublishInvoiceSent publishes invoice.sent event.
func (p *Publisher) PublishInvoiceSent(ctx context.Context, inv domain.CustomerInvoice) {
	p.emit(ctx, "invoice.sent", inv.CorrelationID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishReceivableOverdue publishes receivable.overdue event.
func (p *Publisher) PublishReceivableOverdue(ctx context.Context, inv domain.CustomerInvoice) {
	p.emit(ctx, "receivable.overdue", inv.CorrelationID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishPaymentReceived publishes payment.received event.
func (p *Publisher) PublishPaymentReceived(ctx context.Context, inv domain.CustomerInvoice) {
	p.emit(ctx, "payment.received", inv.CorrelationID, map[string]any{
		"invoice_id":    inv.InvoiceID,
		"amount":        inv.Amount,
		"currency_code": inv.CurrencyCode,
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
		SourceService: "accounts-receivable-svc",
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
