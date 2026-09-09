package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every
// published event carries event name, event version, timestamp, tenant
// ID, legal entity ID, actor ID, correlation ID, source service, and
// payload schema version.
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

// MessageWriter is the one method Publisher needs from *kafka.Writer —
// narrowed to an interface so tests can assert envelope content without a
// live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PRJ-01's own named events: "ProjectCreated; ProjectApproved;
// ProjectActivated; ProjectFinancialProfileChanged; ProjectClosed;
// ProjectReopened."

func (p *Publisher) PublishProjectCreated(ctx context.Context, correlationID, actorID string, pr domain.Project) {
	p.emit(ctx, "project.created", correlationID, pr.TenantID, pr.LegalEntityID, actorID, pr.ProjectID, map[string]any{
		"project_id": pr.ProjectID, "legal_entity_id": pr.LegalEntityID, "project_code": pr.ProjectCode, "status": pr.Status,
	})
}

func (p *Publisher) PublishProjectApproved(ctx context.Context, correlationID, actorID string, pr domain.Project) {
	p.emit(ctx, "project.approved", correlationID, pr.TenantID, pr.LegalEntityID, actorID, pr.ProjectID, map[string]any{
		"project_id": pr.ProjectID, "approved_at": pr.ApprovedAt,
	})
}

func (p *Publisher) PublishProjectActivated(ctx context.Context, correlationID, actorID string, pr domain.Project) {
	p.emit(ctx, "project.activated", correlationID, pr.TenantID, pr.LegalEntityID, actorID, pr.ProjectID, map[string]any{
		"project_id": pr.ProjectID, "activated_at": pr.ActivatedAt,
	})
}

func (p *Publisher) PublishProjectFinancialProfileChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, projectID string) {
	p.emit(ctx, "project.financial_profile.changed", correlationID, tenantID, legalEntityID, actorID, projectID, map[string]any{
		"project_id": projectID,
	})
}

func (p *Publisher) PublishProjectClosed(ctx context.Context, correlationID, actorID string, pr domain.Project) {
	p.emit(ctx, "project.closed", correlationID, pr.TenantID, pr.LegalEntityID, actorID, pr.ProjectID, map[string]any{
		"project_id": pr.ProjectID, "close_reason": pr.CloseReason, "closed_at": pr.ClosedAt,
	})
}

func (p *Publisher) PublishProjectReopened(ctx context.Context, correlationID, actorID string, pr domain.Project) {
	p.emit(ctx, "project.reopened", correlationID, pr.TenantID, pr.LegalEntityID, actorID, pr.ProjectID, map[string]any{
		"project_id": pr.ProjectID, "reopen_reason": pr.ReopenReason, "reopened_at": pr.ReopenedAt,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "project-accounting-svc",
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
			zap.String("event_type", eventType), zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)))
	}
}
