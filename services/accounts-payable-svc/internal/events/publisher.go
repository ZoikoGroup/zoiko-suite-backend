// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
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

// PublishVendorInvoiceReceived corresponds to §10.3's vendor.invoice.received event.
func (p *Publisher) PublishVendorInvoiceReceived(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.received", inv.CorrelationID, map[string]any{
		"invoice_id":      inv.InvoiceID,
		"tenant_id":       inv.TenantID,
		"legal_entity_id": inv.LegalEntityID,
		"vendor_id":       inv.VendorID,
	})
}

// PublishVendorInvoiceValidated corresponds to §10.3's vendor.invoice.validated event.
func (p *Publisher) PublishVendorInvoiceValidated(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.validated", inv.CorrelationID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishVendorInvoiceApproved corresponds to §10.3's vendor.invoice.approved event.
func (p *Publisher) PublishVendorInvoiceApproved(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.approved", inv.CorrelationID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishPaymentRequested corresponds to §10.3's payment.requested event —
// emitted on the APPROVED -> PAYMENT_REQUESTED transition. This is the
// handoff point to a future Treasury/Payments service, which will consume
// this event to actually execute payment — out of scope here.
func (p *Publisher) PublishPaymentRequested(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "payment.requested", inv.CorrelationID, map[string]any{
		"invoice_id": inv.InvoiceID,
		"amount":     inv.Amount,
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
		SourceService: "accounts-payable-svc",
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
