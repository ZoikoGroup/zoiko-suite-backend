package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishEntryCreated(ctx context.Context, correlationID string, entry domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.created", correlationID, map[string]any{
		"intercompany_entry_id":  entry.IntercompanyEntryID,
		"tenant_id":              entry.TenantID,
		"source_legal_entity_id": entry.SourceLegalEntityID,
		"target_legal_entity_id": entry.TargetLegalEntityID,
		"source_journal_id":      entry.SourceJournalID,
		"amount":                 entry.Amount,
		"currency_code":          entry.CurrencyCode,
		"match_status":           entry.MatchStatus,
		"timestamp":              time.Now().UTC(),
	})
}

func (p *Publisher) PublishEntryPosted(ctx context.Context, correlationID string, entry domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.posted", correlationID, map[string]any{
		"intercompany_entry_id":  entry.IntercompanyEntryID,
		"tenant_id":              entry.TenantID,
		"source_legal_entity_id": entry.SourceLegalEntityID,
		"target_legal_entity_id": entry.TargetLegalEntityID,
		"source_journal_id":      entry.SourceJournalID,
		"target_journal_id":      entry.TargetJournalID,
		"amount":                 entry.Amount,
		"currency_code":          entry.CurrencyCode,
		"match_status":           entry.MatchStatus,
		"timestamp":              time.Now().UTC(),
	})
}

func (p *Publisher) PublishMismatchDetected(ctx context.Context, correlationID string, entry domain.IntercompanyEntry, reason string) {
	p.emit(ctx, "intercompany.mismatch.detected", correlationID, map[string]any{
		"intercompany_entry_id":  entry.IntercompanyEntryID,
		"tenant_id":              entry.TenantID,
		"source_legal_entity_id": entry.SourceLegalEntityID,
		"target_legal_entity_id": entry.TargetLegalEntityID,
		"source_journal_id":      entry.SourceJournalID,
		"target_journal_id":      entry.TargetJournalID,
		"mismatch_reason":        reason,
		"timestamp":              time.Now().UTC(),
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
		SourceService: "intercompany-accounting-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if p.producer == nil {
		p.log.Info("simulating publish event in dry mode", zap.String("event_type", eventType))
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
