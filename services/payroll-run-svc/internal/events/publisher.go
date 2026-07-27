package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
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

func (p *Publisher) PublishRunInitiated(ctx context.Context, correlationID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.initiated", correlationID, map[string]any{
		"run_id":           r.RunID,
		"tenant_id":        r.TenantID,
		"legal_entity_id":  r.LegalEntityID,
		"run_number":       r.RunNumber,
		"pay_period_start": r.PayPeriodStart,
		"pay_period_end":   r.PayPeriodEnd,
		"pay_date":         r.PayDate,
		"is_shadow_run":    r.IsShadowRun,
		"status":           r.Status,
		"initiated_at":     r.CreatedAt,
	})
}

func (p *Publisher) PublishRunCalculated(ctx context.Context, correlationID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.calculated", correlationID, map[string]any{
		"run_id":               r.RunID,
		"tenant_id":            r.TenantID,
		"legal_entity_id":      r.LegalEntityID,
		"run_number":           r.RunNumber,
		"total_gross_pay":      r.TotalGrossPay,
		"total_net_pay":        r.TotalNetPay,
		"total_tax_deductions": r.TotalTaxDeductions,
		"employee_count":       r.EmployeeCount,
		"is_shadow_run":        r.IsShadowRun,
		"calculated_at":        r.UpdatedAt,
	})
}

func (p *Publisher) PublishRunCompleted(ctx context.Context, correlationID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.completed", correlationID, map[string]any{
		"run_id":               r.RunID,
		"tenant_id":            r.TenantID,
		"legal_entity_id":      r.LegalEntityID,
		"run_number":           r.RunNumber,
		"total_gross_pay":      r.TotalGrossPay,
		"total_net_pay":        r.TotalNetPay,
		"total_tax_deductions": r.TotalTaxDeductions,
		"employee_count":       r.EmployeeCount,
		"is_shadow_run":        r.IsShadowRun,
		"finalized_at":         r.FinalizedAt,
	})
}

func (p *Publisher) PublishRunBlocked(ctx context.Context, correlationID string, r domain.PayrollRun, reason string) {
	p.emit(ctx, "payroll.run.blocked", correlationID, map[string]any{
		"run_id":          r.RunID,
		"tenant_id":       r.TenantID,
		"legal_entity_id": r.LegalEntityID,
		"run_number":      r.RunNumber,
		"block_reason":    reason,
		"blocked_at":      time.Now().UTC(),
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
		SourceService: "payroll-run-svc",
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