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
	p.emit(ctx, "vendor.invoice.received", inv.CorrelationID, inv.InvoiceID, map[string]any{
		"invoice_id":      inv.InvoiceID,
		"tenant_id":       inv.TenantID,
		"legal_entity_id": inv.LegalEntityID,
		"vendor_id":       inv.VendorID,
	})
}

// PublishVendorInvoiceValidated corresponds to §10.3's vendor.invoice.validated event.
func (p *Publisher) PublishVendorInvoiceValidated(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.validated", inv.CorrelationID, inv.InvoiceID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishVendorInvoiceApproved corresponds to §10.3's vendor.invoice.approved event.
func (p *Publisher) PublishVendorInvoiceApproved(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.approved", inv.CorrelationID, inv.InvoiceID, map[string]any{
		"invoice_id": inv.InvoiceID,
	})
}

// PublishPaymentRequested corresponds to §10.3's payment.requested event —
// emitted on the APPROVED -> PAYMENT_REQUESTED transition. This is the
// handoff point to a future Treasury/Payments service, which will consume
// this event to actually execute payment — out of scope here.
func (p *Publisher) PublishPaymentRequested(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "payment.requested", inv.CorrelationID, inv.InvoiceID, map[string]any{
		"invoice_id":    inv.InvoiceID,
		"amount":        inv.Amount,
		"currency_code": inv.CurrencyCode,
	})
}

// LogOnlyPublisher satisfies the handler's Publisher contract without a broker.
//
// Selected only when KAFKA_BROKERS is explicitly empty, which config.validate
// permits for ENV=local and refuses everywhere else. It exists so a
// single-service local test needs postgres and this service and nothing more —
// previously an empty broker list produced one broker at address "" and every
// publish failed at dial time, which looks identical in the logs to a broker
// that is down.
//
// It logs each event at Info with the full envelope, so the four §10.3 events
// are still observable in order; they are simply not on the backbone.
type LogOnlyPublisher struct {
	log *zap.Logger
}

func NewLogOnlyPublisher(log *zap.Logger) *LogOnlyPublisher {
	log.Warn("no Kafka brokers configured — events will be logged, not published",
		zap.String("consequence", "vendor.invoice.received/validated/approved and payment.requested reach no consumer"))
	return &LogOnlyPublisher{log: log}
}

func (p *LogOnlyPublisher) PublishVendorInvoiceReceived(_ context.Context, inv domain.VendorInvoice) {
	p.record("vendor.invoice.received", inv)
}

func (p *LogOnlyPublisher) PublishVendorInvoiceValidated(_ context.Context, inv domain.VendorInvoice) {
	p.record("vendor.invoice.validated", inv)
}

func (p *LogOnlyPublisher) PublishVendorInvoiceApproved(_ context.Context, inv domain.VendorInvoice) {
	p.record("vendor.invoice.approved", inv)
}

func (p *LogOnlyPublisher) PublishPaymentRequested(_ context.Context, inv domain.VendorInvoice) {
	p.record("payment.requested", inv)
}

func (p *LogOnlyPublisher) record(eventType string, inv domain.VendorInvoice) {
	p.log.Info("event not published (no broker configured)",
		zap.String("event_type", eventType),
		zap.String("invoice_id", inv.InvoiceID),
		zap.String("correlation_id", inv.CorrelationID),
	)
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
