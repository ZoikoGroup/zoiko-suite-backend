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

	"zoiko.io/purchase-request-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.PurchaseRequest carries
// real TenantID/LegalEntityID and per-lifecycle-stage actors
// (RequestedBy/ApprovedBy/RejectedByPrincipalID). No jurisdiction field
// exists on the domain object.
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

// PublishRequestCreated corresponds to §12.8's purchase.request.created event.
func (p *Publisher) PublishRequestCreated(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.created", r.CorrelationID, r.TenantID, r.LegalEntityID, r.RequestedByPrincipalID, r.RequestID, map[string]any{
		"request_id":      r.RequestID,
		"tenant_id":       r.TenantID,
		"legal_entity_id": r.LegalEntityID,
		"amount":          r.Amount,
	})
}

// PublishRequestApproved corresponds to §12.8's purchase.request.approved event.
func (p *Publisher) PublishRequestApproved(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.approved", r.CorrelationID, r.TenantID, r.LegalEntityID, deref(r.ApprovedByPrincipalID), r.RequestID, map[string]any{
		"request_id": r.RequestID,
	})
}

// PublishRequestRejected corresponds to §12.8's purchase.request.rejected event.
func (p *Publisher) PublishRequestRejected(ctx context.Context, r domain.PurchaseRequest) {
	p.emit(ctx, "purchase.request.rejected", r.CorrelationID, r.TenantID, r.LegalEntityID, deref(r.RejectedByPrincipalID), r.RequestID, map[string]any{
		"request_id": r.RequestID,
		"reason":     r.RejectionReason,
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
		SourceService: "purchase-request-svc",
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
