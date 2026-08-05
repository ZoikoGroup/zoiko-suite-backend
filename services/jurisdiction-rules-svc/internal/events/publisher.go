// Package events contains the domain event publisher for jurisdiction-rules-svc.
//
// 03-microservices.md §8.2 lists four published events for this service:
// jurisdiction.rule.updated, jurisdiction.rule.activated,
// jurisdiction.calendar.changed and legal.drift.detected. None of them were
// emitted — the service had no events package at all, so every consumer of
// rule changes (tax, payroll, filing, obligations) had to poll.
//
// Envelope shape mirrors obligations-svc, policy-svc, identity-context-svc
// and tenant-entity-registry-svc exactly.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Event type names, as published on the topic.
//
// jurisdiction.calendar.changed is deliberately absent. §8.2 lists
// "compliance calendar logic" among this service's holdings, but no calendar
// entity exists in its schema — filing due dates and filing_requirements
// live in obligations-svc. Declaring the event name here without a calendar
// to change would advertise a signal that can never fire; adding the entity
// is a separate piece of work, tracked as a gap in services/README.md.
const (
	EventJurisdictionCreated     = "jurisdiction.created"
	EventJurisdictionDeactivated = "jurisdiction.deactivated"
	EventRuleUpdated             = "jurisdiction.rule.updated"
	EventRuleActivated           = "jurisdiction.rule.activated"
	EventLegalDriftDetected      = "legal.drift.detected"
)

// Publisher is the narrow interface the handler depends on, so handler tests
// can assert which events were emitted without a broker.
type Publisher interface {
	PublishJurisdictionCreated(ctx context.Context, j domain.Jurisdiction, correlationID string) error
	PublishJurisdictionDeactivated(ctx context.Context, j domain.Jurisdiction, correlationID string) error
	PublishRuleUpdated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error
	PublishRuleActivated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error
	PublishLegalDriftDetected(ctx context.Context, r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) error
}

// envelope is the standard event wrapper for all events published by this
// service.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// KafkaPublisher implements Publisher against the Kafka event backbone.
// Events are facts, not commands. Published topics are append-only.
type KafkaPublisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

// NewPublisher constructs a KafkaPublisher bound to the given topic and writer.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *KafkaPublisher {
	return &KafkaPublisher{log: log, topic: topic, producer: producer}
}

// PublishJurisdictionCreated announces a new jurisdiction in the registry.
// Callers must only invoke this on a real insert (created=true) — an
// idempotent replay must not re-emit.
func (p *KafkaPublisher) PublishJurisdictionCreated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	return p.emit(ctx, EventJurisdictionCreated, correlationID, map[string]any{
		"jurisdiction_id":         j.JurisdictionID,
		"jurisdiction_code":       j.JurisdictionCode,
		"jurisdiction_name":       j.JurisdictionName,
		"jurisdiction_type":       j.JurisdictionType,
		"parent_jurisdiction_id":  j.ParentJurisdictionID,
		"authority_type":          j.AuthorityType,
		"effective_from":          j.EffectiveFrom,
		"effective_to":            j.EffectiveTo,
		"data_classification":     j.DataClassification,
		"created_by_principal_id": j.CreatedByPrincipalID,
		"created_at":              j.CreatedAt,
	})
}

// PublishJurisdictionDeactivated announces that a jurisdiction has been
// end-dated. Consumers holding entity-jurisdiction assignments against it
// need to know without polling — this is the "jurisdiction.calendar.changed"
// family of signal for the registry itself.
func (p *KafkaPublisher) PublishJurisdictionDeactivated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	return p.emit(ctx, EventJurisdictionDeactivated, correlationID, map[string]any{
		"jurisdiction_id":         j.JurisdictionID,
		"jurisdiction_code":       j.JurisdictionCode,
		"active_flag":             j.ActiveFlag,
		"effective_to":            j.EffectiveTo,
		"updated_at":              j.UpdatedAt,
		"updated_by_principal_id": j.UpdatedByPrincipalID,
	})
}

// PublishRuleUpdated publishes jurisdiction.rule.updated for a newly-created
// rule or any status transition other than the activation case below.
// Callers must only invoke this on a real write, never on a replay.
func (p *KafkaPublisher) PublishRuleUpdated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	return p.emit(ctx, EventRuleUpdated, correlationID, rulePayload(r))
}

// PublishRuleActivated publishes jurisdiction.rule.activated — the signal
// downstream rule engines act on, distinct from a generic update because
// activation is the point a rule starts governing real actions.
func (p *KafkaPublisher) PublishRuleActivated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	return p.emit(ctx, EventRuleActivated, correlationID, rulePayload(r))
}

// PublishLegalDriftDetected publishes legal.drift.detected — the Critical
// Enhancement of §8.2: stored platform rules have diverged from applicable
// legal reality and something must reconcile them.
func (p *KafkaPublisher) PublishLegalDriftDetected(ctx context.Context, r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) error {
	return p.emit(ctx, EventLegalDriftDetected, correlationID, map[string]any{
		"drift_event_id":           e.DriftEventID,
		"jurisdiction_rule_id":     r.JurisdictionRuleID,
		"jurisdiction_id":          r.JurisdictionID,
		"rule_domain":              r.RuleDomain,
		"rule_code":                r.RuleCode,
		"from_state":               e.FromState,
		"to_state":                 e.ToState,
		"reason":                   e.Reason,
		"external_feed_reference":  r.ExternalFeedReference,
		"source_reference":         r.SourceReference,
		"effective_at":             e.EffectiveAt,
		"recorded_by_principal_id": e.RecordedByPrincipalID,
	})
}

// rulePayload is the shared body for the two rule lifecycle events. It
// carries rule identity, applicability metadata and effective dating — a
// consumer must be able to act on the event without a follow-up read.
func rulePayload(r domain.JurisdictionRule) map[string]any {
	return map[string]any{
		"jurisdiction_rule_id":    r.JurisdictionRuleID,
		"jurisdiction_id":         r.JurisdictionID,
		"rule_domain":             r.RuleDomain,
		"rule_code":               r.RuleCode,
		"rule_name":               r.RuleName,
		"rule_status":             r.RuleStatus,
		"legal_drift_state":       r.LegalDriftState,
		"effective_from":          r.EffectiveFrom,
		"effective_to":            r.EffectiveTo,
		"rule_payload":            r.RulePayload,
		"source_reference":        r.SourceReference,
		"external_feed_reference": r.ExternalFeedReference,
		"data_classification":     r.DataClassification,
		"schema_version":          r.SchemaVersion,
	}
}

// emit serialises the payload into the canonical envelope and writes it to
// the Kafka topic set on the Writer (main.go) — not set here, since kafka-go
// rejects a Message that also specifies Topic when the Writer already has one.
//
// The message key is the jurisdiction or rule id where the payload carries
// one, so all events for the same record land on one partition and consumers
// see them in order. Rule status is a state machine; out-of-order delivery
// of updated/activated would let a consumer settle on the wrong state.
func (p *KafkaPublisher) emit(ctx context.Context, eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "jurisdiction-rules-svc",
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{Key: []byte(partitionKey(payload)), Value: data}
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

// partitionKey prefers the rule id, falling back to the jurisdiction id.
func partitionKey(payload map[string]any) string {
	for _, k := range []string{"jurisdiction_rule_id", "jurisdiction_id"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// NoopPublisher drops every event. Used when KAFKA_BROKERS is unset in local
// development, so the service runs standalone without a broker while still
// exercising the same call sites. Never selected in production or staging —
// main.go refuses to start without brokers there.
type NoopPublisher struct {
	log *zap.Logger
}

func NewNoopPublisher(log *zap.Logger) *NoopPublisher { return &NoopPublisher{log: log} }

func (p *NoopPublisher) publish(eventType string) error {
	p.log.Debug("event dropped — no Kafka brokers configured", zap.String("event_type", eventType))
	return nil
}

func (p *NoopPublisher) PublishJurisdictionCreated(context.Context, domain.Jurisdiction, string) error {
	return p.publish(EventJurisdictionCreated)
}

func (p *NoopPublisher) PublishJurisdictionDeactivated(context.Context, domain.Jurisdiction, string) error {
	return p.publish(EventJurisdictionDeactivated)
}

func (p *NoopPublisher) PublishRuleUpdated(context.Context, domain.JurisdictionRule, string) error {
	return p.publish(EventRuleUpdated)
}

func (p *NoopPublisher) PublishRuleActivated(context.Context, domain.JurisdictionRule, string) error {
	return p.publish(EventRuleActivated)
}

func (p *NoopPublisher) PublishLegalDriftDetected(context.Context, domain.JurisdictionRule, domain.DriftEvent, string) error {
	return p.publish(EventLegalDriftDetected)
}
