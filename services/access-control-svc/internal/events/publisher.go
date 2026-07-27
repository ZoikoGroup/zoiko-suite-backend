// Package events provides the Kafka event publisher for access-control-svc.
//
// Events published by this service (per 03-microservices.md §9.4):
//   - role.created
//   - role.updated       (triggered on deactivation or bundle change)
//   - permission.bundle.updated
//
// All events are written to the zoiko.access-control.events Kafka topic.
// On publish failure the operation is logged but NOT rolled back — the
// transactional outbox (event_outbox table) provides the durability guarantee.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
)

// Publisher wraps a kafka.Writer and publishes domain events.
type Publisher struct {
	log    *zap.Logger
	topic  string
	writer *kafka.Writer
}

// NewPublisher creates a Publisher backed by the provided writer.
func NewPublisher(log *zap.Logger, topic string, writer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, writer: writer}
}

type eventEnvelope struct {
	EventType     string      `json:"event_type"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EmittedAt     time.Time   `json:"emitted_at"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	Payload       interface{} `json:"payload"`
}

func (p *Publisher) publish(ctx context.Context, correlationID, eventType string, payload interface{}) error {
	env := eventEnvelope{
		EventType:     eventType,
		SchemaVersion: "1.0",
		SourceService: "access-control-svc",
		EmittedAt:     time.Now().UTC(),
		CorrelationID: correlationID,
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return p.writer.WriteMessages(ctx, kafka.Message{
		Topic: p.topic,
		Key:   []byte(eventType),
		Value: data,
	})
}

// PublishRoleCreated emits a role.created event.
func (p *Publisher) PublishRoleCreated(ctx context.Context, correlationID string, r domain.Role) error {
	err := p.publish(ctx, correlationID, "role.created", r)
	if err != nil {
		p.log.Error("failed to publish role.created", zap.String("role_id", r.RoleID), zap.Error(err))
	}
	return err
}

// PublishRoleUpdated emits a role.updated event (e.g., deactivation).
func (p *Publisher) PublishRoleUpdated(ctx context.Context, correlationID string, r domain.Role) error {
	err := p.publish(ctx, correlationID, "role.updated", r)
	if err != nil {
		p.log.Error("failed to publish role.updated", zap.String("role_id", r.RoleID), zap.Error(err))
	}
	return err
}

// PublishPermissionBundleUpdated emits a permission.bundle.updated event.
func (p *Publisher) PublishPermissionBundleUpdated(ctx context.Context, correlationID string, b domain.PermissionBundle) error {
	err := p.publish(ctx, correlationID, "permission.bundle.updated", b)
	if err != nil {
		p.log.Error("failed to publish permission.bundle.updated", zap.String("bundle_id", b.PermissionBundleID), zap.Error(err))
	}
	return err
}
