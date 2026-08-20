package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/invoice-approval-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.InvoiceApprovalRequest
// carries real TenantID/LegalEntityID and a CreatedByPrincipalID actor for
// the started event; approved/rejected source their actor from the
// deciding principal at each handler call site, since a decision's actor
// is the decider, not the original requester. No jurisdiction field
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

func (p *Publisher) PublishApprovalStarted(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest) {
	p.emit(ctx, "invoice.approval.started", correlationID, req.TenantID, req.LegalEntityID, req.CreatedByPrincipalID, req.ApprovalRequestID, map[string]any{
		"approval_request_id":     req.ApprovalRequestID,
		"tenant_id":               req.TenantID,
		"legal_entity_id":         req.LegalEntityID,
		"invoice_id":              req.InvoiceID,
		"workflow_instance_id":    req.WorkflowInstanceID,
		"invoice_amount":          req.InvoiceAmount,
		"currency_code":           req.CurrencyCode,
		"created_by_principal_id": req.CreatedByPrincipalID,
		"created_at":              req.CreatedAt,
	})
}

func (p *Publisher) PublishApproved(ctx context.Context, correlationID, actorID string, req domain.InvoiceApprovalRequest) {
	p.emit(ctx, "invoice.approved", correlationID, req.TenantID, req.LegalEntityID, actorID, req.ApprovalRequestID, map[string]any{
		"approval_request_id":  req.ApprovalRequestID,
		"tenant_id":            req.TenantID,
		"legal_entity_id":      req.LegalEntityID,
		"invoice_id":           req.InvoiceID,
		"workflow_instance_id": req.WorkflowInstanceID,
		"invoice_amount":       req.InvoiceAmount,
		"currency_code":        req.CurrencyCode,
		"approved_at":          time.Now().UTC(),
	})
}

func (p *Publisher) PublishRejected(ctx context.Context, correlationID, actorID string, req domain.InvoiceApprovalRequest, reason string) {
	p.emit(ctx, "invoice.rejected", correlationID, req.TenantID, req.LegalEntityID, actorID, req.ApprovalRequestID, map[string]any{
		"approval_request_id":  req.ApprovalRequestID,
		"tenant_id":            req.TenantID,
		"legal_entity_id":      req.LegalEntityID,
		"invoice_id":           req.InvoiceID,
		"workflow_instance_id": req.WorkflowInstanceID,
		"rejection_reason":     reason,
		"rejected_at":          time.Now().UTC(),
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
		SourceService: "invoice-approval-svc",
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
