// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event backbone.
// Same posture as every other producer in this platform.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishAuthorizationGranted publishes authorization.granted for a GRANTED decision.
func (p *Publisher) PublishAuthorizationGranted(ctx context.Context, d domain.AccessDecisionLog) error {
	return p.emit("authorization.granted", d.CorrelationID, map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"action_type":        d.ActionType,
		"decision_basis":     d.DecisionBasis,
		"decided_at":         d.DecidedAt,
	})
}

// PublishAuthorizationDenied publishes authorization.denied for a DENIED decision.
func (p *Publisher) PublishAuthorizationDenied(ctx context.Context, d domain.AccessDecisionLog) error {
	return p.emit("authorization.denied", d.CorrelationID, map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"action_type":        d.ActionType,
		"decision_basis":     d.DecisionBasis,
		"decided_at":         d.DecidedAt,
	})
}

// PublishSoDViolationDetected publishes sod.violation.detected — fired in
// addition to authorization.denied specifically when the denial reason was
// an SoD conflict, not a plain no-grant.
func (p *Publisher) PublishSoDViolationDetected(ctx context.Context, d domain.AccessDecisionLog, conflictingAction string) error {
	return p.emit("sod.violation.detected", d.CorrelationID, map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"candidate_action":   d.ActionType,
		"conflicting_action": conflictingAction,
		"decided_at":         d.DecidedAt,
	})
}

func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "authorization-svc",
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{Value: data}
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
