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

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/ActorID/Jurisdiction are `omitempty`, not
// fabricated defaults — a field is only populated when the emitting call
// site actually has real data for it (Jurisdiction is empty here because
// domain.PayrollRun has no jurisdiction field to source it from).
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

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker — *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged. A nil MessageWriter still selects
// the existing "dry mode" logging path in emit() below.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// actorID is the already-authorized principal from the calling handler's
// requirePrincipal() — domain.PayrollRun has no InitiatedBy/CalculatedBy/
// etc. field of its own, so this is the only real source for it.
func (p *Publisher) PublishRunInitiated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.initiated", correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, map[string]any{
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

func (p *Publisher) PublishRunCalculated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.calculated", correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, map[string]any{
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

func (p *Publisher) PublishRunCompleted(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, "payroll.run.completed", correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, map[string]any{
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

func (p *Publisher) PublishRunBlocked(ctx context.Context, correlationID, actorID string, r domain.PayrollRun, reason string) {
	p.emit(ctx, "payroll.run.blocked", correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, map[string]any{
		"run_id":          r.RunID,
		"tenant_id":       r.TenantID,
		"legal_entity_id": r.LegalEntityID,
		"run_number":      r.RunNumber,
		"block_reason":    reason,
		"blocked_at":      time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, actorID, key, tenantID, legalEntityID string, payload map[string]any) {
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
		SourceService: "payroll-run-svc",
		CorrelationID: correlationID,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
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
