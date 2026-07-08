// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// envelope is the standard event wrapper for all events published by this
// service. Mirrors governance-decision-log-svc's/policy-svc's envelope
// shape exactly.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event backbone.
//
// Publishing is stubbed (logged, not written to Kafka) until a
// kafka.Writer is injected — same posture as every other service in this
// repo; there is no real Kafka event backbone wired here yet.
type Publisher struct {
	log   *zap.Logger
	topic string
	// producer *kafka.Writer  — TODO: inject kafka.Writer before Phase 1 exit criteria
}

// NewPublisher constructs a Publisher bound to the given topic.
func NewPublisher(log *zap.Logger, topic string) *Publisher {
	return &Publisher{log: log, topic: topic}
}

// PublishConfigUpdated publishes config.updated for a config entry that
// just underwent a real transition (first write for its scope, or a
// genuinely new value). Callers must not invoke this on the
// idempotent-no-op path (value unchanged).
func (p *Publisher) PublishConfigUpdated(ctx context.Context, entry domain.ConfigEntry, correlationID string) error {
	return p.emit("config.updated", correlationID, map[string]any{
		"config_id":               entry.ConfigID,
		"key":                     entry.Key,
		"value":                   entry.Value,
		"environment":             entry.Environment,
		"tenant_id":               entry.TenantID,
		"effective_from":          entry.EffectiveFrom,
		"created_by_principal_id": entry.CreatedByPrincipalID,
	})
}

// PublishFeatureFlagUpdated publishes feature_flag.updated for a feature
// flag that just underwent a real transition. Same not-on-no-op rule as
// PublishConfigUpdated.
func (p *Publisher) PublishFeatureFlagUpdated(ctx context.Context, flag domain.FeatureFlag, correlationID string) error {
	return p.emit("feature_flag.updated", correlationID, map[string]any{
		"flag_id":                 flag.FlagID,
		"key":                     flag.Key,
		"enabled":                 flag.Enabled,
		"environment":             flag.Environment,
		"tenant_id":               flag.TenantID,
		"rollout_percentage":      flag.RolloutPercentage,
		"effective_from":          flag.EffectiveFrom,
		"created_by_principal_id": flag.CreatedByPrincipalID,
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
		SourceService: "configuration-feature-flag-svc",
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
