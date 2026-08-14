// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
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
// Publish failures are logged, never returned/propagated — same posture as
// every other producer in this platform (an outbox pattern handles
// redelivery; DB writes are never rolled back on a publish failure).
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishStatementIngested corresponds to §10.5's statement.ingested event.
func (p *Publisher) PublishStatementIngested(ctx context.Context, l domain.StatementLine) {
	p.emit(ctx, "statement.ingested", l.CorrelationID, l.StatementLineID, map[string]any{
		"statement_line_id": l.StatementLineID,
		"tenant_id":         l.TenantID,
		"legal_entity_id":   l.LegalEntityID,
		"bank_account_id":   l.BankAccountID,
		"amount":            l.Amount,
	})
}

// PublishReconciliationMatched corresponds to §10.5's reconciliation.matched event.
func (p *Publisher) PublishReconciliationMatched(ctx context.Context, l domain.StatementLine) {
	p.emit(ctx, "reconciliation.matched", l.CorrelationID, l.StatementLineID, map[string]any{
		"statement_line_id":  l.StatementLineID,
		"matched_journal_id": l.MatchedJournalID,
	})
}

// PublishReconciliationExceptionRaised corresponds to §10.5's
// reconciliation.exception.raised event.
func (p *Publisher) PublishReconciliationExceptionRaised(ctx context.Context, l domain.StatementLine) {
	p.emit(ctx, "reconciliation.exception.raised", l.CorrelationID, l.StatementLineID, map[string]any{
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
func (p *Publisher) PublishReconciliationCompleted(ctx context.Context, correlationID, tenantID, bankAccountID, statementDate string) {
	p.emit(ctx, "reconciliation.completed", correlationID, bankAccountID, map[string]any{
		"tenant_id":       tenantID,
		"bank_account_id": bankAccountID,
		"statement_date":  statementDate,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "bank-reconciliation-svc",
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
