// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
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

// PublishJournalCreated corresponds to §10.1's journal.created event.
func (p *Publisher) PublishJournalCreated(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.created", h.CorrelationID, map[string]any{
		"journal_id":      h.JournalID,
		"tenant_id":       h.TenantID,
		"legal_entity_id": h.LegalEntityID,
		"fiscal_period":   h.FiscalPeriod,
	})
}

// PublishJournalValidated corresponds to §10.1's journal.validated event.
func (p *Publisher) PublishJournalValidated(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.validated", h.CorrelationID, map[string]any{
		"journal_id": h.JournalID,
	})
}

// PublishJournalPosted corresponds to §10.1's journal.posted event — emitted
// on the VALIDATED -> FINALIZED transition.
func (p *Publisher) PublishJournalPosted(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.posted", h.CorrelationID, map[string]any{
		"journal_id": h.JournalID,
	})
}

// PublishJournalReversed corresponds to §10.1's journal.reversed event.
// reversingJournalID is the new journal created to carry the reversing
// entries — the original journal's own rows are never edited.
func (p *Publisher) PublishJournalReversed(ctx context.Context, h domain.JournalHeader, reversingJournalID string) {
	p.emit(ctx, "journal.reversed", h.CorrelationID, map[string]any{
		"journal_id":            h.JournalID,
		"reversing_journal_id":  reversingJournalID,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "general-ledger-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
