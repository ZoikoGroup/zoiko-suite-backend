package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/ActorID/Jurisdiction are `omitempty`, not
// fabricated defaults — a field is only populated when the emitting call
// site actually has real data for it (Jurisdiction is empty here because
// domain.PayrollRun has no jurisdiction field to source it from).
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

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker — *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged. A nil MessageWriter still selects
// the existing "dry mode" logging path in emit() below.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// actorID is the already-authorized principal from the calling handler's
// requirePrincipal() — domain.PayrollRun has no InitiatedBy/CalculatedBy/
// etc. field of its own, so this is the only real source for it.
func (p *Publisher) PublishRunInitiated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, RunInitiated, correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, runPayload(RunInitiated, r, ""))
}

func (p *Publisher) PublishRunCalculated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, RunCalculated, correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, runPayload(RunCalculated, r, ""))
}

func (p *Publisher) PublishRunCompleted(ctx context.Context, correlationID, actorID string, r domain.PayrollRun) {
	p.emit(ctx, RunCompleted, correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, runPayload(RunCompleted, r, ""))
}

func (p *Publisher) PublishRunBlocked(ctx context.Context, correlationID, actorID string, r domain.PayrollRun, reason string) {
	p.emit(ctx, RunBlocked, correlationID, actorID, r.RunID, r.TenantID, r.LegalEntityID, runPayload(RunBlocked, r, reason))
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, actorID, key, tenantID, legalEntityID string, payload map[string]any) {
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
		SourceService: "payroll-run-svc",
		CorrelationID: correlationID,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
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
