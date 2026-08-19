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

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/ActorID/Jurisdiction are `omitempty`, not
// fabricated defaults — a field is only populated when the emitting call
// site actually has real data for it (e.g. Jurisdiction is empty here
// because domain.JournalHeader has no jurisdiction field to source it
// from; inventing one would be worse than omitting it).
type envelope struct {
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	Jurisdiction  string          `json:"jurisdiction,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// eventContext carries the envelope-level fields that vary per call site,
// as opposed to the payload-level business fields.
type eventContext struct {
	TenantID      string
	LegalEntityID string
	ActorID       string
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker — *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged.
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

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishJournalCreated corresponds to §10.1's journal.created event.
func (p *Publisher) PublishJournalCreated(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.created", h.CorrelationID, h.JournalID,
		eventContext{TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, ActorID: h.CreatedByPrincipalID},
		map[string]any{
			"journal_id":      h.JournalID,
			"tenant_id":       h.TenantID,
			"legal_entity_id": h.LegalEntityID,
			"fiscal_period":   h.FiscalPeriod,
		})
}

// PublishJournalValidated corresponds to §10.1's journal.validated event.
func (p *Publisher) PublishJournalValidated(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.validated", h.CorrelationID, h.JournalID,
		eventContext{TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, ActorID: deref(h.ValidatedByPrincipalID)},
		map[string]any{
			"journal_id": h.JournalID,
		})
}

// PublishJournalPosted corresponds to §10.1's journal.posted event — emitted
// on the VALIDATED -> FINALIZED transition.
func (p *Publisher) PublishJournalPosted(ctx context.Context, h domain.JournalHeader) {
	p.emit(ctx, "journal.posted", h.CorrelationID, h.JournalID,
		eventContext{TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, ActorID: deref(h.PostedByPrincipalID)},
		map[string]any{
			"journal_id": h.JournalID,
		})
}

// PublishJournalReversed corresponds to §10.1's journal.reversed event.
// reversingJournalID is the new journal created to carry the reversing
// entries — the original journal's own rows are never edited.
func (p *Publisher) PublishJournalReversed(ctx context.Context, h domain.JournalHeader, reversingJournalID string) {
	p.emit(ctx, "journal.reversed", h.CorrelationID, h.JournalID,
		eventContext{TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, ActorID: deref(h.ReversedByPrincipalID)},
		map[string]any{
			"journal_id":           h.JournalID,
			"reversing_journal_id": reversingJournalID,
		})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, ec eventContext, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "general-ledger-svc",
		CorrelationID: correlationID,
		TenantID:      ec.TenantID,
		LegalEntityID: ec.LegalEntityID,
		ActorID:       ec.ActorID,
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
