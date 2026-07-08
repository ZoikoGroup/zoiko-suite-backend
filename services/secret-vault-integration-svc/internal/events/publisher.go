// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"time"

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
// backbone. Publishing is stubbed (logged, not written to Kafka) until a
// kafka.Writer is injected — same posture as every other service here.
type Publisher struct {
	log   *zap.Logger
	topic string
	// producer *kafka.Writer  — TODO: inject kafka.Writer before Phase 1 exit criteria
}

// NewPublisher constructs a Publisher bound to the given topic.
func NewPublisher(log *zap.Logger, topic string) *Publisher {
	return &Publisher{log: log, topic: topic}
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
// Kafka. Stub: logs structured JSON until kafka.Writer is injected.
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

	// TODO: publish to Kafka topic
	// msg := kafka.Message{Topic: p.topic, Value: data}
	// if err := p.producer.WriteMessages(ctx, msg); err != nil { ... outbox retry ... }

	p.log.Info("event emitted (stub — wire Kafka writer)",
		zap.String("event_type", eventType),
		zap.String("correlation_id", correlationID),
		zap.ByteString("payload", data),
	)
	return nil
}
