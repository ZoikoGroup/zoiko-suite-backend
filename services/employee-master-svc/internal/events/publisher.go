package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/employee-master-svc/internal/domain"
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

func (p *Publisher) PublishEmployeeCreated(ctx context.Context, correlationID string, emp domain.Employee) {
	p.emit(ctx, "employee.created", correlationID, map[string]any{
		"employee_id":     emp.EmployeeID,
		"tenant_id":       emp.TenantID,
		"legal_entity_id": emp.LegalEntityID,
		"first_name":      emp.FirstName,
		"last_name":       emp.LastName,
		"email":           emp.Email,
		"worker_type":     emp.WorkerType,
		"status":          emp.Status,
		"created_at":      emp.CreatedAt,
	})
}

func (p *Publisher) PublishEmployeeHired(ctx context.Context, correlationID string, emp domain.Employee) {
	p.emit(ctx, "employee.hired", correlationID, map[string]any{
		"employee_id":     emp.EmployeeID,
		"tenant_id":       emp.TenantID,
		"legal_entity_id": emp.LegalEntityID,
		"hire_date":       emp.HireDate,
		"worker_type":     emp.WorkerType,
		"effective_from":  emp.EffectiveFrom,
	})
}

func (p *Publisher) PublishEmployeeUpdated(ctx context.Context, correlationID string, emp domain.Employee) {
	p.emit(ctx, "employee.updated", correlationID, map[string]any{
		"employee_id":         emp.EmployeeID,
		"tenant_id":           emp.TenantID,
		"legal_entity_id":     emp.LegalEntityID,
		"employee_number":     emp.EmployeeNumber,
		"first_name":          emp.FirstName,
		"last_name":           emp.LastName,
		"phone":               emp.Phone,
		"job_title":           emp.JobTitle,
		"department_id":       emp.DepartmentID,
		"manager_employee_id": emp.ManagerEmployeeID,
		"worker_type":         emp.WorkerType,
		"updated_at":          emp.UpdatedAt,
	})
}

func (p *Publisher) PublishStatusChanged(ctx context.Context, correlationID string, emp domain.Employee, oldStatus string) {
	p.emit(ctx, "employee.status.changed", correlationID, map[string]any{
		"employee_id":     emp.EmployeeID,
		"tenant_id":       emp.TenantID,
		"legal_entity_id": emp.LegalEntityID,
		"old_status":      oldStatus,
		"new_status":      emp.Status,
		"updated_at":      time.Now().UTC(),
	})
}

func (p *Publisher) PublishEmployeeTerminated(ctx context.Context, correlationID string, emp domain.Employee) {
	p.emit(ctx, "employee.terminated", correlationID, map[string]any{
		"employee_id":      emp.EmployeeID,
		"tenant_id":        emp.TenantID,
		"legal_entity_id":  emp.LegalEntityID,
		"termination_date": emp.TerminationDate,
		"effective_to":     emp.EffectiveTo,
		"terminated_at":    time.Now().UTC(),
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
		SourceService: "employee-master-svc",
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