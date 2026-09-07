// Package events contains the domain event publisher and consumer.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. Not every event this service
// publishes has a tenant/legal-entity/actor to source from (e.g. a failed
// resolution may not have resolved a tenant at all) — those fields are
// correctly omitted per-event rather than fabricated when the underlying
// call site has nothing real to put there.
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
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements EventPublisher against the Kafka event backbone.
//
// Evidence obligation: every resolution (success AND failure) produces a
// durable event. Publish calls are non-blocking from Resolve()'s perspective
// — callers invoke them in goroutines. The producer uses an outbox-retry
// pattern (TODO: implement outbox) for delivery guarantees.
//
// Gap 1 (fixed): PublishX methods return error so callers can detect and
// log publish failures rather than silently discarding them.
//
// Gap 2 (known, not yet fixed): there is no WaitGroup or outbox drain on
// process shutdown. In-flight goroutines may be lost on SIGTERM. Tracked
// as a Phase 1 exit-criteria gap — see PR description.
//
// Events are facts, not commands. Published topics are append-only.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishContextResolved(
	ctx context.Context,
	principalID, tenantID, legalEntityID, sessionContextID, correlationID string,
) error {
	return p.emit(ctx, "identity.context.resolved", tenantID, legalEntityID, principalID, correlationID, sessionContextID, map[string]any{
		"principal_id":       principalID,
		"tenant_id":          tenantID,
		"legal_entity_id":    legalEntityID,
		"session_context_id": sessionContextID,
		"correlation_id":     correlationID,
	})
}

func (p *Publisher) PublishResolutionFailed(
	ctx context.Context,
	subject, correlationID, reason string,
) error {
	return p.emit(ctx, "identity.context.resolution_failed", "", "", "", correlationID, subject, map[string]any{
		"principal_id_or_subject": subject,
		"correlation_id":          correlationID,
		"failure_reason":          reason,
	})
}

func (p *Publisher) PublishSessionInvalidated(
	ctx context.Context,
	sessionContextID, principalID string,
	reason domain.InvalidationReason,
	correlationID string,
) error {
	return p.emit(ctx, "session.invalidated", "", "", principalID, correlationID, sessionContextID, map[string]any{
		"session_context_id":  sessionContextID,
		"principal_id":        principalID,
		"invalidation_reason": reason,
		"correlation_id":      correlationID,
	})
}

func (p *Publisher) PublishRiskSignalUnavailable(ctx context.Context, principalID, correlationID string) error {
	return p.emit(ctx, "session.risk.changed", "", "", principalID, correlationID, principalID, map[string]any{
		"principal_id":   principalID,
		"new_posture":    string(domain.TrustPostureStandard),
		"signal_source":  "UNAVAILABLE",
		"correlation_id": correlationID,
	})
}

func (p *Publisher) PublishPrincipalStatusChanged(
	ctx context.Context,
	principalID, tenantID string,
	newStatus domain.PrincipalStatus,
	actorID, correlationID string,
) error {
	return p.emit(ctx, "principal.status.changed", tenantID, "", actorID, correlationID, principalID, map[string]any{
		"principal_id":   principalID,
		"tenant_id":      tenantID,
		"new_status":     string(newStatus),
		"actor":          actorID,
		"correlation_id": correlationID,
	})
}

// PublishAuthenticationSucceeded records that a human proved possession of a
// principal's credential.
//
// Distinct from identity.context.resolved: that event says an envelope was
// issued, this one says a password was verified. They are usually seconds
// apart but they answer different questions, and collapsing them would make
// "someone logged in but never obtained an envelope" — a resolution that
// failed on tenant, entity or trust posture AFTER a correct password —
// invisible in the event stream.
func (p *Publisher) PublishAuthenticationSucceeded(
	ctx context.Context,
	principalID, tenantID, correlationID string,
) error {
	return p.emit(ctx, "identity.authentication.succeeded", tenantID, "", principalID, correlationID, principalID, map[string]any{
		"principal_id":    principalID,
		"tenant_id":       tenantID,
		"credential_type": "PASSWORD",
		"correlation_id":  correlationID,
	})
}

// PublishAuthenticationFailed records a rejected attempt.
//
// subject is the email as supplied. It is emitted even when it matches no
// principal, because a stream of failures against addresses that do not exist
// is the signature of an enumeration sweep, and dropping those events would
// hide exactly the attack the constant-time verification path is defending
// against. principalID is empty in that case rather than fabricated.
func (p *Publisher) PublishAuthenticationFailed(
	ctx context.Context,
	subject, principalID, tenantID, correlationID, reason string,
) error {
	return p.emit(ctx, "identity.authentication.failed", tenantID, "", principalID, correlationID, subject, map[string]any{
		"subject":         subject,
		"principal_id":    principalID,
		"tenant_id":       tenantID,
		"credential_type": "PASSWORD",
		"failure_reason":  reason,
		"correlation_id":  correlationID,
	})
}

// emit serialises the payload and writes to the Kafka topic.
// Returns an error so callers can detect failures (Gap 1 fix).
func (p *Publisher) emit(ctx context.Context, eventType, tenantID, legalEntityID, actorID, correlationID, key string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "identity-context-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	// Topic is set on the Writer itself (main.go), not here — kafka-go
	// rejects a Message that also specifies Topic when the Writer already has one.
	msg := kafka.Message{Key: []byte(key), Value: data}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
	)
	return nil
}
