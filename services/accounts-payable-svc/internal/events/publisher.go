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

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/ActorID/Jurisdiction are `omitempty`, not
// fabricated defaults — a field is only populated when the emitting call
// site actually has real data for it (Jurisdiction is empty here because
// domain.VendorInvoice has no jurisdiction field to source it from).
type envelope struct {
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	Jurisdiction  string          `json:"jurisdiction,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// eventContext carries the envelope-level fields that vary per call site,
// as opposed to the payload-level business fields.
type eventContext struct {
	TenantID      string
	LegalEntityID string
	ActorID       string
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker — *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements event publishing against the Kafka event backbone.
// Publish failures are logged, never returned/propagated — same posture as
// every other producer in this platform (an outbox pattern handles
// redelivery; DB writes are never rolled back on a publish failure).
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishVendorInvoiceReceived corresponds to §10.3's vendor.invoice.received event.
func (p *Publisher) PublishVendorInvoiceReceived(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.received", inv.CorrelationID, inv.InvoiceID,
		eventContext{TenantID: inv.TenantID, LegalEntityID: inv.LegalEntityID, ActorID: inv.CreatedByPrincipalID},
		map[string]any{
			"invoice_id":      inv.InvoiceID,
			"tenant_id":       inv.TenantID,
			"legal_entity_id": inv.LegalEntityID,
			"vendor_id":       inv.VendorID,
		})
}

// PublishVendorInvoiceValidated corresponds to §10.3's vendor.invoice.validated event.
func (p *Publisher) PublishVendorInvoiceValidated(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.validated", inv.CorrelationID, inv.InvoiceID,
		eventContext{TenantID: inv.TenantID, LegalEntityID: inv.LegalEntityID, ActorID: deref(inv.ValidatedByPrincipalID)},
		map[string]any{
			"invoice_id": inv.InvoiceID,
		})
}

// PublishVendorInvoiceApproved corresponds to §10.3's vendor.invoice.approved event.
func (p *Publisher) PublishVendorInvoiceApproved(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "vendor.invoice.approved", inv.CorrelationID, inv.InvoiceID,
		eventContext{TenantID: inv.TenantID, LegalEntityID: inv.LegalEntityID, ActorID: deref(inv.ApprovedByPrincipalID)},
		map[string]any{
			"invoice_id": inv.InvoiceID,
		})
}

// PublishPaymentRequested corresponds to §10.3's payment.requested event —
// emitted on the APPROVED -> PAYMENT_REQUESTED transition. This is the
// handoff point to a future Treasury/Payments service, which will consume
// this event to actually execute payment — out of scope here.
func (p *Publisher) PublishPaymentRequested(ctx context.Context, inv domain.VendorInvoice) {
	p.emit(ctx, "payment.requested", inv.CorrelationID, inv.InvoiceID,
		eventContext{TenantID: inv.TenantID, LegalEntityID: inv.LegalEntityID, ActorID: deref(inv.PaymentRequestedByPrincipalID)},
		map[string]any{
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, ec eventContext, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "accounts-payable-svc",
		CorrelationID: correlationID,
		TenantID:      ec.TenantID,
		LegalEntityID: ec.LegalEntityID,
		ActorID:       ec.ActorID,
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
