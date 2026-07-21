package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-tax-svc/internal/domain"
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

func (p *Publisher) PublishTaxCalculated(ctx context.Context, correlationID string, calc domain.TaxCalculationRecord) {
	p.emit(ctx, "payroll.tax.calculated", correlationID, map[string]any{
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

func (p *Publisher) PublishTaxAdjusted(ctx context.Context, correlationID string, calc domain.TaxCalculationRecord) {
	p.emit(ctx, "payroll.tax.adjusted", correlationID, map[string]any{
		"calculation_id":   calc.CalculationID,
		"tenant_id":        calc.TenantID,
		"employee_id":      calc.EmployeeID,
		"total_tax_amount": calc.TotalTaxAmount,
		"adjusted_at":      time.Now().UTC(),
	})
}

func (p *Publisher) PublishTaxException(ctx context.Context, correlationID, calcID, reason string) {
	p.emit(ctx, "payroll.tax.exception.detected", correlationID, map[string]any{
		"calculation_id": calcID,
		"reason":         reason,
		"detected_at":     time.Now().UTC(),
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
		SourceService: "payroll-tax-svc",
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