// Package events contains the domain event publisher for this service.
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

// PublishEntryCreated corresponds to §10.6's intercompany.entry.created event.
func (p *Publisher) PublishEntryCreated(ctx context.Context, e domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.created", e.CorrelationID, map[string]any{
		"intercompany_entry_id":  e.IntercompanyEntryID,
		"tenant_id":              e.TenantID,
		"source_legal_entity_id": e.SourceLegalEntityID,
		"target_legal_entity_id": e.TargetLegalEntityID,
		"amount":                 e.Amount,
	})
}

// PublishEntryPosted corresponds to §10.6's intercompany.entry.posted event —
// emitted once both sides are verified and the entry reaches MATCHED.
func (p *Publisher) PublishEntryPosted(ctx context.Context, e domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.posted", e.CorrelationID, map[string]any{
		"intercompany_entry_id":   e.IntercompanyEntryID,
		"target_journal_entry_id": e.TargetJournalEntryID,
	})
}

// PublishMismatchDetected corresponds to §10.6's
// intercompany.mismatch.detected event — emitted when an attempted match
// fails verification against general-ledger-svc.
func (p *Publisher) PublishMismatchDetected(ctx context.Context, e domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.mismatch.detected", e.CorrelationID, map[string]any{
		"intercompany_entry_id": e.IntercompanyEntryID,
		"reason":                e.MismatchReason,
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
