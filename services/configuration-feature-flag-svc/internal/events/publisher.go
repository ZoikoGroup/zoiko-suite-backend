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

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.ConfigEntry and
// domain.FeatureFlag are independently-nullable-scoped like
// kill-switch-registry-svc — TenantID is nil for a global default, so
// tenant_id is correctly omitted in that case rather than fabricated.
// Neither struct has a legal_entity_id or jurisdiction field: config and
// feature flags are environment/tenant-scoped, not legal-entity-scoped.
// CreatedByPrincipalID is a genuine actor field.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
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

// Publisher implements event publishing against the Kafka event backbone.
//
// Publishing is stubbed (logged, not written to Kafka) until a
// kafka.Writer is injected — same posture as every other service in this
// repo; there is no real Kafka event backbone wired here yet.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured,
	// in which case emit drops the event and says so. main.go refuses to
	// start with a nil producer in production or staging.
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic and writer.
// A nil producer makes every publish a logged no-op — see the struct comment.
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

// PublishConfigUpdated publishes config.updated for a config entry that
// just underwent a real transition (first write for its scope, or a
// genuinely new value). Callers must not invoke this on the
// idempotent-no-op path (value unchanged).
func (p *Publisher) PublishConfigUpdated(ctx context.Context, entry domain.ConfigEntry, correlationID string) error {
	return p.emit(ctx, "config.updated", correlationID, deref(entry.TenantID), entry.CreatedByPrincipalID, map[string]any{
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
	return p.emit(ctx, "feature_flag.updated", correlationID, deref(flag.TenantID), flag.CreatedByPrincipalID, map[string]any{
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
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, actorID string, payload map[string]any) error {
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
		SourceService: "configuration-feature-flag-svc",
		TenantID:      tenantID,
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
