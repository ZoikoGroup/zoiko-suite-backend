package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-exceptions-svc/internal/domain"
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

func (p *Publisher) PublishExceptionRaised(ctx context.Context, correlationID string, e domain.PayrollException) {
	p.emit(ctx, "payroll.exception.raised", correlationID, map[string]any{
		"exception_id":   e.ExceptionID,
		"tenant_id":      e.TenantID,
		"payroll_run_id": e.PayrollRunID,
		"employee_id":    e.EmployeeID,
		"exception_code": e.ExceptionCode,
		"severity":       e.Severity,
		"description":    e.Description,
		"raised_at":      e.CreatedAt,
	})
}

func (p *Publisher) PublishExceptionResolved(ctx context.Context, correlationID string, e domain.PayrollException) {
	p.emit(ctx, "payroll.exception.resolved", correlationID, map[string]any{
		"exception_id":     e.ExceptionID,
		"tenant_id":        e.TenantID,
		"payroll_run_id":   e.PayrollRunID,
		"status":           e.Status,
		"resolution_notes": e.ResolutionNotes,
		"resolved_by":      e.ResolvedBy,
		"resolved_at":      e.ResolvedAt,
	})
}

func (p *Publisher) PublishBlockerFlagged(ctx context.Context, correlationID, payrollRunID string, blockerCount int) {
	p.emit(ctx, "payroll.blocker.flagged", correlationID, map[string]any{
		"payroll_run_id": payrollRunID,
		"blocker_count":  blockerCount,
		"flagged_at":     time.Now().UTC(),
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
		SourceService: "payroll-exceptions-svc",
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