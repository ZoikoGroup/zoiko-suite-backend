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

	"zoiko.io/authorization-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.AccessDecisionLog carries a
// real LegalEntityID and a natural actor (PrincipalID — the principal
// whose access was evaluated). It has no tenant_id or jurisdiction field
// (RecordAccessDecision never persists a tenant scope), so both are
// correctly omitted rather than fabricated.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
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

// Publisher implements event publishing against the Kafka event backbone.
// Same posture as every other producer in this platform.
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

// PublishAuthorizationGranted publishes authorization.granted for a GRANTED decision.
func (p *Publisher) PublishAuthorizationGranted(ctx context.Context, d domain.AccessDecisionLog) error {
	return p.emit(ctx, "authorization.granted", d.CorrelationID, d.LegalEntityID, d.PrincipalID, d.AccessDecisionID, map[string]any{
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
	return p.emit(ctx, "authorization.denied", d.CorrelationID, d.LegalEntityID, d.PrincipalID, d.AccessDecisionID, map[string]any{
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
	return p.emit(ctx, "sod.violation.detected", d.CorrelationID, d.LegalEntityID, d.PrincipalID, d.AccessDecisionID, map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"candidate_action":   d.ActionType,
		"conflicting_action": conflictingAction,
		"decided_at":         d.DecidedAt,
	})
}

// PublishBreakGlassStarted publishes security.break_glass.started (ZS-IAM-001 §23).
func (p *Publisher) PublishBreakGlassStarted(ctx context.Context, session domain.BreakGlassSession) error {
	return p.emit(ctx, "security.break_glass.started", "", "", session.PrincipalID, session.SessionID, map[string]any{
		"session_id":        session.SessionID,
		"tenant_id":         session.TenantID,
		"principal_id":      session.PrincipalID,
		"incident_id":       session.IncidentID,
		"reason":            session.Reason,
		"requested_actions": session.RequestedActions,
		"status":            session.Status,
		"expires_at":        session.ExpiresAt,
	})
}

// PublishBreakGlassEnded publishes security.break_glass.ended (ZS-IAM-001 §23).
func (p *Publisher) PublishBreakGlassEnded(ctx context.Context, session domain.BreakGlassSession) error {
	return p.emit(ctx, "security.break_glass.ended", "", "", session.PrincipalID, session.SessionID, map[string]any{
		"session_id":   session.SessionID,
		"tenant_id":    session.TenantID,
		"principal_id": session.PrincipalID,
		"incident_id":  session.IncidentID,
		"status":       session.Status,
		"revoked_at":   session.RevokedAt,
		"revoked_by":   session.RevokedBy,
	})
}

// PublishSupportSessionStarted publishes support.session.started (ZS-IAM-001 §23).
func (p *Publisher) PublishSupportSessionStarted(ctx context.Context, session domain.SupportSession) error {
	return p.emit(ctx, "support.session.started", "", "", session.SupportOperatorID, session.SessionID, map[string]any{
		"session_id":              session.SessionID,
		"tenant_id":               session.TenantID,
		"support_operator_id":     session.SupportOperatorID,
		"ticket_ref":              session.TicketRef,
		"purpose":                 session.Purpose,
		"read_only":               session.ReadOnly,
		"allow_bulk_export":       session.AllowBulkExport,
		"allowed_actions":         session.AllowedActions,
		"status":                  session.Status,
		"expires_at":              session.ExpiresAt,
		"tenant_consent_obtained": session.TenantConsentObtained,
	})
}

// PublishSupportSessionEnded publishes support.session.ended (ZS-IAM-001 §23).
func (p *Publisher) PublishSupportSessionEnded(ctx context.Context, session domain.SupportSession) error {
	return p.emit(ctx, "support.session.ended", "", "", session.SupportOperatorID, session.SessionID, map[string]any{
		"session_id":          session.SessionID,
		"tenant_id":           session.TenantID,
		"support_operator_id": session.SupportOperatorID,
		"ticket_ref":          session.TicketRef,
		"status":              session.Status,
		"revoked_at":          session.RevokedAt,
		"revoked_by":          session.RevokedBy,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, legalEntityID, actorID, key string, payload map[string]any) error {
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
		SourceService: "authorization-svc",
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{Key: []byte(key), Value: data}
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
