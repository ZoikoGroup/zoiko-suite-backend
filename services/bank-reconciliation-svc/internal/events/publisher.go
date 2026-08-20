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

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.StatementLine carries real
// TenantID/LegalEntityID and per-lifecycle-stage actors
// (MatchedBy/FlaggedByPrincipalID); it has no jurisdiction field.
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
// Publish failures are logged, never returned/propagated — same posture as
// every other producer in this platform (an outbox pattern handles
// redelivery; DB writes are never rolled back on a publish failure).
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PublishStatementIngested corresponds to §10.5's statement.ingested event.
func (p *Publisher) PublishStatementIngested(ctx context.Context, l domain.StatementLine, actorID string) {
	p.emit(ctx, "statement.ingested", l.CorrelationID, l.TenantID, l.LegalEntityID, actorID, l.StatementLineID, map[string]any{
		"statement_line_id": l.StatementLineID,
		"tenant_id":         l.TenantID,
		"legal_entity_id":   l.LegalEntityID,
		"bank_account_id":   l.BankAccountID,
		"amount":            l.Amount,
	})
}

// PublishReconciliationMatched corresponds to §10.5's reconciliation.matched event.
func (p *Publisher) PublishReconciliationMatched(ctx context.Context, l domain.StatementLine) {
	p.emit(ctx, "reconciliation.matched", l.CorrelationID, l.TenantID, l.LegalEntityID, deref(l.MatchedByPrincipalID), l.StatementLineID, map[string]any{
		"statement_line_id":  l.StatementLineID,
		"matched_journal_id": l.MatchedJournalID,
	})
}

// PublishReconciliationExceptionRaised corresponds to §10.5's
// reconciliation.exception.raised event.
func (p *Publisher) PublishReconciliationExceptionRaised(ctx context.Context, l domain.StatementLine) {
	p.emit(ctx, "reconciliation.exception.raised", l.CorrelationID, l.TenantID, l.LegalEntityID, deref(l.FlaggedByPrincipalID), l.StatementLineID, map[string]any{
		"statement_line_id": l.StatementLineID,
		"reason":            l.ExceptionReason,
	})
}

// PublishReconciliationCompleted corresponds to §10.5's
// reconciliation.completed event — emitted once every line for a given bank
// account + statement date is no longer UNMATCHED.
//
// correlationID is threaded through from the request's X-Correlation-ID.
// This event used to be emitted with an empty one while every other event
// here carried the value, so the completion of a statement was the single
// thing in this service's event stream that could not be traced back to the
// request that caused it.
func (p *Publisher) PublishReconciliationCompleted(ctx context.Context, correlationID, tenantID, actorID, bankAccountID, statementDate string) {
	p.emit(ctx, "reconciliation.completed", correlationID, tenantID, "", actorID, bankAccountID, map[string]any{
		"tenant_id":       tenantID,
		"bank_account_id": bankAccountID,
		"statement_date":  statementDate,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "bank-reconciliation-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
