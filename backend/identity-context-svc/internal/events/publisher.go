// Package events contains the domain event publisher and consumer.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// envelope is the standard event wrapper.
// Every published event includes these mandatory fields per doctrine
// (01-backend.md §09.1 event design principles).
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	Payload       json.RawMessage `json:"payload"`
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
	log   *zap.Logger
	topic string
	// producer *kafka.Writer  — TODO: inject kafka.Writer before Phase 1 exit criteria
}

func NewPublisher(log *zap.Logger, topic string) *Publisher {
	return &Publisher{log: log, topic: topic}
}

func (p *Publisher) PublishContextResolved(
	ctx context.Context,
	principalID, tenantID, legalEntityID, sessionContextID, correlationID string,
) error {
	return p.emit("identity.context.resolved", map[string]any{
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
	return p.emit("identity.context.resolution_failed", map[string]any{
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
	return p.emit("session.invalidated", map[string]any{
		"session_context_id":  sessionContextID,
		"principal_id":        principalID,
		"invalidation_reason": reason,
		"correlation_id":      correlationID,
	})
}

func (p *Publisher) PublishRiskSignalUnavailable(ctx context.Context, principalID, correlationID string) error {
	return p.emit("session.risk.changed", map[string]any{
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
	return p.emit("principal.status.changed", map[string]any{
		"principal_id":   principalID,
		"tenant_id":      tenantID,
		"new_status":     string(newStatus),
		"actor":          actorID,
		"correlation_id": correlationID,
	})
}

// emit serialises the payload and writes to the Kafka topic.
// Returns an error so callers can detect failures (Gap 1 fix).
// Stub: logs structured JSON until kafka.Writer is injected.
func (p *Publisher) emit(eventType string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "identity-context-svc",
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	// TODO: publish to Kafka
	// msg := kafka.Message{Topic: p.topic, Value: data}
	// if err := p.producer.WriteMessages(context.Background(), msg); err != nil {
	//     return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	// }

	p.log.Info("event emitted (stub — wire Kafka producer)",
		zap.String("event_type", eventType),
		zap.ByteString("payload", data),
	)
	return nil
}
