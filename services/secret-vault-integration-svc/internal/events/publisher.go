// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.SecretLease is
// independently-nullable-scoped (nil TenantID/LegalEntityID for a
// platform-wide secret) — nil is correctly omitted rather than
// fabricated. secret.access.requested fires before any lease exists, so
// it has no tenant/legal-entity scope to report yet. secret.rotation.
// completed sources its actor from the caller-supplied
// RotatedByPrincipalID.
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

// Publisher implements event publishing against the Kafka event
// backbone — same posture as every other service here.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured,
	// in which case emit drops the event and says so. main.go refuses to
	// start with a nil producer in production or staging.
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer — storing that directly into the
	// MessageWriter interface field would make the interface itself
	// non-nil, defeating the p.producer == nil check in emit().
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PublishAccessRequested publishes secret.access.requested — fires on
// every broker request regardless of outcome (context.md §7.2 step 1).
func (p *Publisher) PublishAccessRequested(ctx context.Context, secretPath, requestedByPrincipalID, correlationID string) error {
	return p.emit(ctx, "secret.access.requested", correlationID, "", "", requestedByPrincipalID, map[string]any{
		"secret_path":               secretPath,
		"requested_by_principal_id": requestedByPrincipalID,
	})
}

// PublishAccessGranted publishes secret.access.granted for a newly
// granted lease. Callers must only invoke this on a real grant
// (created=true) — an idempotent replay must not re-emit the event.
func (p *Publisher) PublishAccessGranted(ctx context.Context, lease domain.SecretLease, correlationID string) error {
	return p.emit(ctx, "secret.access.granted", correlationID, deref(lease.TenantID), deref(lease.LegalEntityID), lease.RequestedByPrincipalID, map[string]any{
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
func (p *Publisher) PublishRotationCompleted(ctx context.Context, secretPolicyID, secretPath, rotatedByPrincipalID string, revokedLeaseCount int, correlationID string) error {
	return p.emit(ctx, "secret.rotation.completed", correlationID, "", "", rotatedByPrincipalID, map[string]any{
		"secret_policy_id":    secretPolicyID,
		"secret_path":         secretPath,
		"revoked_lease_count": revokedLeaseCount,
	})
}

// emit serialises the payload into the canonical envelope and writes to
// Kafka. With no producer (local development, no brokers) it drops the
// event and says so at Debug.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "secret-vault-integration-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
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
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
	return nil
}
