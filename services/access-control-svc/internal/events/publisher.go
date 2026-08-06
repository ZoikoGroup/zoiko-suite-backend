package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/access-control-svc/internal/domain"
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

func (p *Publisher) PublishRoleCreated(ctx context.Context, r domain.RoleDefinition) {
	p.emit(ctx, "role.created", r.CorrelationID, map[string]any{
		"role_definition_id": r.RoleDefinitionID,
		"role_code":          r.RoleCode,
	})
}

func (p *Publisher) PublishRoleUpdated(ctx context.Context, r domain.RoleDefinition) {
	p.emit(ctx, "role.updated", r.CorrelationID, map[string]any{
		"role_definition_id": r.RoleDefinitionID,
		"status":             r.Status,
	})
}

func (p *Publisher) PublishBundleUpdated(ctx context.Context, b domain.PermissionBundleDef) {
	p.emit(ctx, "permission.bundle.updated", b.CorrelationID, map[string]any{
		"bundle_id":          b.BundleID,
		"role_definition_id": b.RoleDefinitionID,
		"permitted_actions":  b.PermittedActions,
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
		SourceService: "access-control-svc",
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
