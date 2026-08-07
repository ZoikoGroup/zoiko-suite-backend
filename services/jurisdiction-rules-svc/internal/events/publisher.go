// Package events contains the domain event publisher for jurisdiction-rules-svc.
//
// Per docs/architecture/03-microservices.md §8.2, this service publishes
// jurisdiction.rule.updated, jurisdiction.rule.activated,
// jurisdiction.calendar.changed, and legal.drift.detected. Before this
// package existed, this service had NO event publishing of any kind — not
// even a logged stub — despite doctrine.md §3.4/§17.3 treating the event
// backbone as non-optional infrastructure for every plane.
//
// Scope of what's wired here: jurisdiction.rule.updated (on rule creation)
// and jurisdiction.rule.activated (on a real DRAFT->ACTIVE transition).
// jurisdiction.calendar.changed is deliberately NOT published — this
// service owns no calendar entity at all (see known-gaps.md, "Open:
// jurisdiction-rules-svc owns no compliance calendar"), so there is nothing
// real to signal. legal.drift.detected is also deliberately NOT published
// — no code path anywhere in this service ever sets LegalDriftState to
// DRIFTED (it is only ever set to CURRENT at creation time); drift
// detection is meant to be driven by an external regulatory feed
// (external-data-feed-svc, per 03-microservices.md §16.6's "feeds legal
// drift detection"), which doesn't exist as an integration yet. Publishing
// either event now would advertise a signal no consumer could ever receive
// — the same reasoning docs/architecture/known-gaps.md gives for not
// inventing schema.registered on schema-registry-svc.
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

// envelope is the standard event wrapper shared across ZoikoSuite services.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event backbone.
// producer may be nil (dry-run mode) — emit() logs instead of writing.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

// NewPublisher constructs a Publisher bound to the given topic and producer.
// Pass a nil producer for dry-run/log-only mode.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishRuleUpdated fires whenever a jurisdiction rule is created or its
// content changes. Callers must only invoke this for a genuine change —
// an idempotent replay of an existing rule must not re-emit the event.
func (p *Publisher) PublishRuleUpdated(ctx context.Context, correlationID string, rule domain.JurisdictionRule) error {
	return p.emit(ctx, "jurisdiction.rule.updated", correlationID, map[string]any{
		"jurisdiction_rule_id": rule.JurisdictionRuleID,
		"jurisdiction_id":      rule.JurisdictionID,
		"rule_domain":          rule.RuleDomain,
		"rule_code":            rule.RuleCode,
		"rule_status":          rule.RuleStatus,
		"effective_from":       rule.EffectiveFrom,
		"effective_to":         rule.EffectiveTo,
	})
}

// PublishRuleActivated fires when a rule genuinely transitions into ACTIVE
// status (DRAFT->ACTIVE). Callers must only invoke this on a real
// transition, not an idempotent no-op replay of an already-ACTIVE rule.
func (p *Publisher) PublishRuleActivated(ctx context.Context, correlationID string, rule domain.JurisdictionRule) error {
	return p.emit(ctx, "jurisdiction.rule.activated", correlationID, map[string]any{
		"jurisdiction_rule_id": rule.JurisdictionRuleID,
		"jurisdiction_id":      rule.JurisdictionID,
		"rule_domain":          rule.RuleDomain,
		"rule_code":            rule.RuleCode,
		"effective_from":       rule.EffectiveFrom,
	})
}

// emit serialises the payload into the canonical envelope and writes it to
// Kafka. If no producer was configured (dry-run mode), it logs instead.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
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
		return err
	}

	if p.producer == nil {
		p.log.Info("simulating publish event in dry mode",
			zap.String("event_type", eventType),
			zap.String("correlation_id", correlationID),
		)
		return nil
	}

	if err := p.producer.WriteMessages(ctx, kafka.Message{Value: data}); err != nil {
		return fmt.Errorf("publish %s: %w", eventType, err)
	}
	return nil
}
