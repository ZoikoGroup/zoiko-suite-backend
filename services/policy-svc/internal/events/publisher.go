// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
)

// envelope is the standard event wrapper for all events published by this
// service. Mirrors governance-decision-log-svc's, identity-context-svc's,
// and tenant-entity-registry-svc's envelope shape exactly.
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
// Events are facts, not commands. Published topics are append-only. Same
// posture as identity-context-svc and tenant-entity-registry-svc's producers.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

// NewPublisher constructs a Publisher bound to the given topic and Kafka writer.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishPolicyCreated publishes policy.created for a newly-created
// policy. Callers must only invoke this on the first insert
// (created=true) — a replayed idempotent POST must not re-emit the event.
func (p *Publisher) PublishPolicyCreated(ctx context.Context, policy domain.Policy, correlationID string) error {
	return p.emit("policy.created", correlationID, map[string]any{
		"policy_id":               policy.PolicyID,
		"policy_code":             policy.PolicyCode,
		"policy_name":             policy.PolicyName,
		"policy_type":             policy.PolicyType,
		"created_by_principal_id": policy.CreatedByPrincipalID,
		"created_at":              policy.CreatedAt,
	})
}

// PublishPolicyUpdated publishes policy.updated for a newly-created policy
// version. In this schema a "policy update" is always a new version row —
// there is no UPDATE on policies or policy_versions. Callers must only
// invoke this on the first insert (created=true).
func (p *Publisher) PublishPolicyUpdated(ctx context.Context, version domain.PolicyVersion, correlationID string) error {
	return p.emit("policy.updated", correlationID, map[string]any{
		"policy_version_id":       version.PolicyVersionID,
		"policy_id":               version.PolicyID,
		"tenant_id":               version.TenantID,
		"legal_entity_id":         version.LegalEntityID,
		"effective_from":          version.EffectiveFrom,
		"effective_to":            version.EffectiveTo,
		"version_status":          version.VersionStatus,
		"created_by_principal_id": version.CreatedByPrincipalID,
	})
}

// PublishVersionActivated publishes policy.version.activated for a
// version that just transitioned to ACTIVE.
func (p *Publisher) PublishVersionActivated(ctx context.Context, version domain.PolicyVersion, correlationID string) error {
	return p.emit("policy.version.activated", correlationID, map[string]any{
		"policy_version_id":         version.PolicyVersionID,
		"policy_id":                 version.PolicyID,
		"tenant_id":                 version.TenantID,
		"legal_entity_id":           version.LegalEntityID,
		"effective_from":            version.EffectiveFrom,
		"activated_by_principal_id": version.ActivatedByPrincipalID,
		"activated_at":              version.ActivatedAt,
	})
}

// PublishRuleRetired publishes policy.rule.retired for a version that just
// transitioned to SUPERSEDED (as a side effect of activating a newer
// version in the same scope — see Store.ActivateVersion) or, in a future
// batch, RETIRED via a dedicated retire endpoint.
func (p *Publisher) PublishRuleRetired(ctx context.Context, version domain.PolicyVersion, correlationID string) error {
	return p.emit("policy.rule.retired", correlationID, map[string]any{
		"policy_version_id": version.PolicyVersionID,
		"policy_id":         version.PolicyID,
		"version_status":    version.VersionStatus,
	})
}

// emit serialises the payload into the canonical envelope and writes it to
// the Kafka topic set on the Writer (main.go) — not set here, since kafka-go
// rejects a Message that also specifies Topic when the Writer already has one.
func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "policy-svc",
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
