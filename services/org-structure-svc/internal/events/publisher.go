package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.Position and domain.
// OrgAssignment both carry real TenantID/LegalEntityID but no actor field
// of their own, so actor_id is threaded through explicitly from each
// handler's already-verified principalID. No jurisdiction field exists
// anywhere in this service.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
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

func (p *Publisher) PublishPositionCreated(ctx context.Context, correlationID, actorID string, pos domain.Position) {
	p.emit(ctx, "position.created", correlationID, pos.TenantID, pos.LegalEntityID, actorID, pos.PositionID, map[string]any{
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

func (p *Publisher) PublishEmployeeAssigned(ctx context.Context, correlationID, actorID string, assign domain.OrgAssignment) {
	p.emit(ctx, "employee.assigned", correlationID, assign.TenantID, assign.LegalEntityID, actorID, assign.AssignmentID, map[string]any{
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

func (p *Publisher) PublishOrgStructureChanged(ctx context.Context, correlationID, tenantID, legalEntityID, actorID, eventType, entityID string) {
	p.emit(ctx, "org.structure.changed", correlationID, tenantID, legalEntityID, actorID, entityID, map[string]any{
		"change_type": eventType,
		"entity_id":   entityID,
		"changed_at":  time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) {
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
		SourceService: "org-structure-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
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
