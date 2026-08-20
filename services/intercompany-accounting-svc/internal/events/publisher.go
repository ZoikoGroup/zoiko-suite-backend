package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.IntercompanyEntry is
// inherently two-entity (SourceLegalEntityID and TargetLegalEntityID) — an
// intercompany entry has no single canonical legal_entity_id, so the
// envelope's legal_entity_id slot is left empty rather than arbitrarily
// picking one side; both remain explicit named fields inside the payload,
// as they already were. actor_id is threaded through explicitly from each
// handler's already-verified principalID (the domain object has no actor
// field of its own). No jurisdiction field exists anywhere in this
// service.
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

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the
	// interface itself non-nil, defeating the p.producer == nil check in
	// emit(). Keep the field genuinely nil in that case.
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

func (p *Publisher) PublishEntryCreated(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.created", correlationID, entry.TenantID, actorID, entry.IntercompanyEntryID, map[string]any{
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

func (p *Publisher) PublishEntryPosted(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry) {
	p.emit(ctx, "intercompany.entry.posted", correlationID, entry.TenantID, actorID, entry.IntercompanyEntryID, map[string]any{
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

func (p *Publisher) PublishMismatchDetected(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry, reason string) {
	p.emit(ctx, "intercompany.mismatch.detected", correlationID, entry.TenantID, actorID, entry.IntercompanyEntryID, map[string]any{
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

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, actorID, key string, payload map[string]any) {
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
		SourceService: "intercompany-accounting-svc",
		TenantID:      tenantID,
		ActorID:       actorID,
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
