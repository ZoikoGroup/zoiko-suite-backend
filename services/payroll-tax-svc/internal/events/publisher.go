package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-tax-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.TaxCalculationRecord
// carries a real JurisdictionCode but no legal_entity_id or actor field
// of its own — the employee's legal entity is resolved at each handler
// call site (via resolveEmployeeEntity) and threaded through explicitly,
// alongside the already-verified principalID as actor_id.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	Jurisdiction  string          `json:"jurisdiction,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the
	// interface itself non-nil, defeating the p.producer == nil check in
	// emit(). Keep the field genuinely nil in that case.
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

func (p *Publisher) PublishTaxCalculated(ctx context.Context, correlationID, legalEntityID, actorID string, calc domain.TaxCalculationRecord) {
	p.emit(ctx, "payroll.tax.calculated", correlationID, calc.TenantID, legalEntityID, calc.JurisdictionCode, actorID, calc.CalculationID, map[string]any{
		"calculation_id":    calc.CalculationID,
		"tenant_id":         calc.TenantID,
		"payroll_run_id":    calc.PayrollRunID,
		"employee_id":       calc.EmployeeID,
		"jurisdiction_code": calc.JurisdictionCode,
		"taxable_basis":     calc.TaxableBasis,
		"total_tax_amount":  calc.TotalTaxAmount,
		"engine_type":       calc.EngineType,
		"calculated_at":     calc.CreatedAt,
	})
}

func (p *Publisher) PublishTaxAdjusted(ctx context.Context, correlationID, legalEntityID, actorID string, calc domain.TaxCalculationRecord) {
	p.emit(ctx, "payroll.tax.adjusted", correlationID, calc.TenantID, legalEntityID, calc.JurisdictionCode, actorID, calc.CalculationID, map[string]any{
		"calculation_id":   calc.CalculationID,
		"tenant_id":        calc.TenantID,
		"employee_id":      calc.EmployeeID,
		"total_tax_amount": calc.TotalTaxAmount,
		"adjusted_at":      time.Now().UTC(),
	})
}

func (p *Publisher) PublishTaxException(ctx context.Context, correlationID, tenantID, legalEntityID, actorID, calcID, reason string) {
	p.emit(ctx, "payroll.tax.exception.detected", correlationID, tenantID, legalEntityID, "", actorID, calcID, map[string]any{
		"calculation_id": calcID,
		"reason":         reason,
		"detected_at":    time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, jurisdiction, actorID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "payroll-tax-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		Jurisdiction:  jurisdiction,
		ActorID:       actorID,
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
