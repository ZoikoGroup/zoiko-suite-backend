package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/procurement-workflow-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.ProcurementCase carries
// real TenantID/LegalEntityID and RequestedByPrincipalID as the natural
// actor for the requested/approval-started events; procurement.completed
// sources its actor from the principal who issued the purchase order,
// threaded through explicitly since that may not be the original
// requester. No jurisdiction field exists on the domain object.
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

// PublishRequested fires when a procurement case is first created, whether
// or not the spend-check that runs synchronously during creation allows it
// — the event marks the request happening, not any particular outcome.
func (p *Publisher) PublishRequested(ctx context.Context, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.requested", c.CorrelationID, c.TenantID, c.LegalEntityID, c.RequestedByPrincipalID, c.CaseID, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
		"category":        c.Category,
		"amount":          c.Amount,
		"currency_code":   c.CurrencyCode,
		"status":          c.Status,
	})
}

// PublishApprovalStarted fires only when the spend-check allows the case to
// enter the approval stage — a spend-blocked case never reaches this event.
func (p *Publisher) PublishApprovalStarted(ctx context.Context, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.approval.started", c.CorrelationID, c.TenantID, c.LegalEntityID, c.RequestedByPrincipalID, c.CaseID, map[string]any{
		"case_id":         c.CaseID,
		"legal_entity_id": c.LegalEntityID,
	})
}

// PublishCompleted fires once the real purchase order has been issued and
// the case reaches its terminal COMPLETED state.
func (p *Publisher) PublishCompleted(ctx context.Context, actorID string, c domain.ProcurementCase) {
	p.emit(ctx, "procurement.completed", c.CorrelationID, c.TenantID, c.LegalEntityID, actorID, c.CaseID, map[string]any{
		"case_id":           c.CaseID,
		"legal_entity_id":   c.LegalEntityID,
		"purchase_order_id": c.PurchaseOrderID,
		"amount":            c.Amount,
		"currency_code":     c.CurrencyCode,
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
		SourceService: "procurement-workflow-svc",
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
