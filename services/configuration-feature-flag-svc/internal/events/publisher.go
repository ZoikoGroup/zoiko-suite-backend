// Package events builds and publishes this service domain events.
//
// The package is split in two on purpose, and the split is the whole design:
//
//   - Build/ConfigUpdated/FeatureFlagUpdated turn a state change into a sealed
//     envelope. The store calls these INSIDE the transaction that records the
//     change and enqueues the result in event_outbox, so the fact that
//     something changed is committed atomically with the change itself.
//   - Publish hands already-built envelopes to Kafka. internal/outbox calls it
//     from a background relay, where a failure is a retry rather than a loss.
//
// Before the outbox existed these were one act: a Kafka write from the handler
// AFTER the commit, with the error logged and discarded. A broker hiccup during
// a config write therefore left the new version correctly recorded, the
// operator correctly told it was saved, and every consumer still holding the
// PREVIOUS value — with nothing anywhere reporting a fault. On this service
// that is the worst possible failure shape: the stale value a consumer keeps
// serving is perfectly valid, just superseded, so nothing downstream can
// detect it either.
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

// Contract constants, asserted against asyncapi.yaml by scripts/audit.sh.
const (
	EventVersion  = "1.0"
	SchemaVersion = "1.0"
	SourceService = "configuration-feature-flag-svc"

	// TypeConfigUpdated is emitted only when POST /v1/config performs a real
	// transition — a first value for the scope, or a genuinely different one.
	// Never on the idempotent no-op path: re-asserting a value that is already
	// in force is not a new fact, and a consumer that invalidated its cache on
	// one would be doing so for nothing.
	TypeConfigUpdated = "config.updated"
	// TypeFeatureFlagUpdated is the same rule for POST /v1/flags.
	TypeFeatureFlagUpdated = "feature_flag.updated"
)

// envelope is this platform event contract (Doc 03 §19): every published event
// carries event name, event version, timestamp, tenant ID, actor ID,
// correlation ID, source service, and payload schema version.
//
// domain.ConfigEntry and domain.FeatureFlag are independently-nullable-scoped
// like kill-switch-registry-svc — TenantID is nil for a global default, so
// tenant_id is correctly OMITTED in that case rather than fabricated. Neither
// struct has a legal_entity_id or jurisdiction field: config and feature flags
// are environment/tenant-scoped, not legal-entity-scoped.
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

// Outbound is one sealed envelope on its way to the outbox.
//
// Key becomes the Kafka partition key. It is the aggregate id — config_id or
// flag_id — not the correlation id, which is what it used to be: keying on the
// correlation id put two changes to the SAME config key on different
// partitions whenever they arrived on different requests, so a consumer
// replaying them could apply an older value after a newer one and end up
// serving the superseded value permanently.
type Outbound struct {
	EventType string
	Key       string
	Body      []byte
}

// Build marshals one envelope.
func Build(eventType, correlationID, tenantID, actorID, key string, payload map[string]any) (Outbound, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	body, err := json.Marshal(envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md event_id collision writeup.
		EventID:       "evt-" + uuid.NewString(),
		EventType:     eventType,
		EventVersion:  EventVersion,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: SchemaVersion,
		SourceService: SourceService,
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	})
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s envelope: %w", eventType, err)
	}
	return Outbound{EventType: eventType, Key: key, Body: body}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ConfigUpdated builds config.updated for a config entry that just underwent a
// real transition.
//
// scope_is_global is in the payload rather than left for a consumer to infer
// from a null tenant_id. The two facts read very differently — "this value
// changed for one organisation" and "the default every organisation without
// its own value reads has changed" — and a consumer that treats a missing
// tenant_id as "unknown tenant" rather than "all tenants" silently ignores the
// wider of the two.
func ConfigUpdated(entry domain.ConfigEntry, correlationID string) (Outbound, error) {
	return Build(TypeConfigUpdated, correlationID, deref(entry.TenantID), entry.CreatedByPrincipalID, entry.ConfigID, map[string]any{
		"config_id":               entry.ConfigID,
		"key":                     entry.Key,
		"value":                   entry.Value,
		"environment":             entry.Environment,
		"tenant_id":               entry.TenantID,
		"scope_is_global":         entry.TenantID == nil,
		"effective_from":          entry.EffectiveFrom,
		"created_by_principal_id": entry.CreatedByPrincipalID,
	})
}

// FeatureFlagUpdated builds feature_flag.updated. Same not-on-no-op rule as
// ConfigUpdated.
func FeatureFlagUpdated(flag domain.FeatureFlag, correlationID string) (Outbound, error) {
	return Build(TypeFeatureFlagUpdated, correlationID, deref(flag.TenantID), flag.CreatedByPrincipalID, flag.FlagID, map[string]any{
		"flag_id":                 flag.FlagID,
		"key":                     flag.Key,
		"enabled":                 flag.Enabled,
		"environment":             flag.Environment,
		"tenant_id":               flag.TenantID,
		"scope_is_global":         flag.TenantID == nil,
		"rollout_percentage":      flag.RolloutPercentage,
		"effective_from":          flag.EffectiveFrom,
		"created_by_principal_id": flag.CreatedByPrincipalID,
	})
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so tests can assert envelope content without
// a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes sealed envelopes to the Kafka event backbone.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured, in
	// which case Publish drops the batch and says so. main.go refuses to start
	// with a nil producer in production or staging.
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic and writer.
// A nil producer makes every publish a logged no-op — see the struct comment.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer — storing that directly into the
	// MessageWriter interface field would make the interface itself non-nil,
	// defeating the p.producer == nil check in Publish.
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

// Publish writes a whole batch in one call.
//
// One call, not a loop over WriteMessages. kafka-go batches internally and its
// BatchTimeout is what ends a partial batch, so a per-message loop waits that
// timeout out once per event — which turns a backlog of 200 into 200 sequential
// waits and makes the relay slower the further behind it gets.
func (p *Publisher) Publish(ctx context.Context, msgs []kafka.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if p.producer == nil {
		p.log.Info("simulating publish — no Kafka brokers configured", zap.Int("events", len(msgs)))
		return nil
	}
	// Topic is set on the Writer, not the Message — kafka-go rejects a Message
	// carrying a Topic when the Writer already has one.
	if err := p.producer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write to %s: %w", p.topic, err)
	}
	p.log.Debug("events published", zap.String("topic", p.topic), zap.Int("events", len(msgs)))
	return nil
}
