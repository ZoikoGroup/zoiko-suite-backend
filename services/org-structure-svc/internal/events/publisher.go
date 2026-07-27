package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
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

func (p *Publisher) PublishPositionCreated(ctx context.Context, correlationID string, pos domain.Position) {
	p.emit(ctx, "position.created", correlationID, map[string]any{
		"position_id":     pos.PositionID,
		"tenant_id":       pos.TenantID,
		"legal_entity_id": pos.LegalEntityID,
		"department_id":   pos.DepartmentID,
		"title":           pos.Title,
		"code":            pos.Code,
		"job_level":       pos.JobLevel,
		"max_headcount":   pos.MaxHeadcount,
		"created_at":      pos.CreatedAt,
	})
}

func (p *Publisher) PublishEmployeeAssigned(ctx context.Context, correlationID string, assign domain.OrgAssignment) {
	p.emit(ctx, "employee.assigned", correlationID, map[string]any{
		"assignment_id":       assign.AssignmentID,
		"tenant_id":           assign.TenantID,
		"employee_id":         assign.EmployeeID,
		"department_id":       assign.DepartmentID,
		"position_id":         assign.PositionID,
		"manager_employee_id": assign.ManagerEmployeeID,
		"effective_from":      assign.EffectiveFrom,
		"assigned_at":         assign.CreatedAt,
	})
}

func (p *Publisher) PublishOrgStructureChanged(ctx context.Context, correlationID string, eventType, entityID string) {
	p.emit(ctx, "org.structure.changed", correlationID, map[string]any{
		"change_type": eventType,
		"entity_id":   entityID,
		"changed_at":   time.Now().UTC(),
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
		SourceService: "org-structure-svc",
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