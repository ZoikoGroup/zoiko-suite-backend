package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/invoice-approval-svc/internal/domain"
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

func (p *Publisher) PublishApprovalStarted(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest) {
	p.emit(ctx, "invoice.approval.started", correlationID, map[string]any{
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

func (p *Publisher) PublishApproved(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest) {
	p.emit(ctx, "invoice.approved", correlationID, map[string]any{
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

func (p *Publisher) PublishRejected(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest, reason string) {
	p.emit(ctx, "invoice.rejected", correlationID, map[string]any{
		"approval_request_id":  req.ApprovalRequestID,
		"tenant_id":            req.TenantID,
		"legal_entity_id":      req.LegalEntityID,
		"invoice_id":           req.InvoiceID,
		"workflow_instance_id": req.WorkflowInstanceID,
		"rejection_reason":     reason,
		"rejected_at":          time.Now().UTC(),
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
		SourceService: "invoice-approval-svc",
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