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

	"zoiko.io/workflow-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/ActorID/Jurisdiction are `omitempty`, not
// fabricated defaults — Jurisdiction is empty here because
// domain.WorkflowInstance has no jurisdiction field to source it from.
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
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements event publishing against the Kafka event backbone.
// Same posture as every other producer in this platform.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishWorkflowStarted(ctx context.Context, w domain.WorkflowInstance) error {
	return p.emit(ctx, "workflow.started", w.CorrelationID, w.TenantID, w.LegalEntityID, w.InitiatedBy, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"tenant_id":            w.TenantID,
		"legal_entity_id":      w.LegalEntityID,
		"workflow_type":        w.WorkflowType,
		"initiated_by":         w.InitiatedBy,
		"started_at":           w.StartedAt,
	})
}

// actorID is the already-verified req.ActorPrincipalID from the calling
// handler — the principal the request says acted, which
// CheckApprovalAllowed already confirmed is entitled to act on this stage.
func (p *Publisher) PublishApprovalGranted(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage, actorID string) error {
	return p.emit(ctx, "approval.granted", w.CorrelationID, w.TenantID, w.LegalEntityID, actorID, map[string]any{
		"workflow_instance_id":  w.WorkflowInstanceID,
		"stage_order":           stage.StageOrder,
		"approver_principal_id": stage.ApproverPrincipalID,
	})
}

func (p *Publisher) PublishApprovalRejected(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage, actorID string) error {
	return p.emit(ctx, "approval.rejected", w.CorrelationID, w.TenantID, w.LegalEntityID, actorID, map[string]any{
		"workflow_instance_id":  w.WorkflowInstanceID,
		"stage_order":           stage.StageOrder,
		"approver_principal_id": stage.ApproverPrincipalID,
	})
}

func (p *Publisher) PublishWorkflowEscalated(ctx context.Context, w domain.WorkflowInstance, actorID string) error {
	return p.emit(ctx, "workflow.escalated", w.CorrelationID, w.TenantID, w.LegalEntityID, actorID, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"current_stage":        w.CurrentStage,
	})
}

// actorID is the last actor who caused this terminal transition — whoever
// gave the final APPROVE/REJECT, or whoever cancelled the workflow. Not
// necessarily the same principal across every call to this method.
func (p *Publisher) PublishWorkflowCompleted(ctx context.Context, w domain.WorkflowInstance, actorID string) error {
	return p.emit(ctx, "workflow.completed", w.CorrelationID, w.TenantID, w.LegalEntityID, actorID, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"workflow_status":      w.WorkflowStatus,
		"completed_at":         w.CompletedAt,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "workflow-svc",
		CorrelationID: correlationID,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	// Assign a stable per-event UUID and surface it as an X-Event-ID Kafka header.
	// This is the key that workflow-history-svc (and audit-event-store-svc) use
	// as their primary dedup key via INSERT … ON CONFLICT (event_id) DO NOTHING.
	// Using a header (rather than embedding only in the JSON payload) lets the
	// consumer extract the ID before deserialising the payload, matching the
	// pattern expected by internal/kafka/runner.go's extractEventID().
	//
	// Producer-retry safety: if the caller retries emit() after a transient
	// Kafka write failure, a NEW uuid is generated for the retry — the previous
	// call may or may not have reached the broker. This is the correct posture
	// for an at-least-once producer: the consumer's ON CONFLICT dedup absorbs
	// broker-level re-deliveries (same offset, same ID), while producer retries
	// that succeed on a second attempt are inherently new logical publications.
	eventID := uuid.New().String()
	msg := kafka.Message{
		Value: data,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(eventID)},
		},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_id", eventID),
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
	return nil
}
