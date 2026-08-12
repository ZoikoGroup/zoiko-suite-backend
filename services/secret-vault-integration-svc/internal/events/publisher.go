// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// envelope is the standard event wrapper for all events published by
// this service. Mirrors every other service's envelope shape exactly.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event
// backbone — same posture as every other service here.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured,
	// in which case emit drops the event and says so. main.go refuses to
	// start with a nil producer in production or staging.
	producer *kafka.Writer
}

// NewPublisher constructs a Publisher bound to the given topic.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishAccessRequested publishes secret.access.requested — fires on
// every broker request regardless of outcome (context.md §7.2 step 1).
func (p *Publisher) PublishAccessRequested(ctx context.Context, secretPath, requestedByPrincipalID, correlationID string) error {
	return p.emit("secret.access.requested", correlationID, map[string]any{
		"secret_path":               secretPath,
		"requested_by_principal_id": requestedByPrincipalID,
	})
}

// PublishAccessGranted publishes secret.access.granted for a newly
// granted lease. Callers must only invoke this on a real grant
// (created=true) — an idempotent replay must not re-emit the event.
func (p *Publisher) PublishAccessGranted(ctx context.Context, lease domain.SecretLease, correlationID string) error {
	return p.emit("secret.access.granted", correlationID, map[string]any{
		"lease_id":                  lease.LeaseID,
		"secret_class":              lease.SecretClass,
		"secret_path":               lease.SecretPath,
		"requested_by_principal_id": lease.RequestedByPrincipalID,
		"tenant_id":                 lease.TenantID,
		"legal_entity_id":           lease.LegalEntityID,
		"expires_at":                lease.ExpiresAt,
	})
}

// PublishRotationCompleted publishes secret.rotation.completed for a
// real rotation (not an idempotent replay of the same request_id).
func (p *Publisher) PublishRotationCompleted(ctx context.Context, secretPolicyID, secretPath string, revokedLeaseCount int, correlationID string) error {
	return p.emit("secret.rotation.completed", correlationID, map[string]any{
		"secret_policy_id":    secretPolicyID,
		"secret_path":         secretPath,
		"revoked_lease_count": revokedLeaseCount,
	})
}

// emit serialises the payload into the canonical envelope and writes to
// Kafka. With no producer (local development, no brokers) it drops the
// event and says so at Debug.
func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "secret-vault-integration-svc",
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	if p.producer == nil {
		p.log.Debug("event dropped — no Kafka brokers configured",
			zap.String("event_type", eventType),
			zap.String("correlation_id", correlationID),
		)
		return nil
	}

	// Topic is set on the Writer, not the Message — kafka-go rejects a
	// Message carrying a Topic when the Writer already has one.
	msg := kafka.Message{Key: []byte(correlationID), Value: data}
	if err := p.producer.WriteMessages(context.Background(), msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
	return nil
}
