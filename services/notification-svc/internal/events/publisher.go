package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/notification-svc/internal/domain"
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

func (p *Publisher) PublishSent(ctx context.Context, correlationID string, n domain.Notification) {
	p.emit(ctx, "notification.sent", correlationID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"source_event_type":      n.SourceEventType,
		"source_reference":       n.SourceReference,
		"sent_at":                n.SentAt,
	})
}

func (p *Publisher) PublishFailed(ctx context.Context, correlationID string, n domain.Notification, reason string) {
	p.emit(ctx, "notification.failed", correlationID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"failure_reason":         reason,
		"failed_at":              time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "notification-svc",
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
