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

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher implements event publishing against the Kafka event backbone.
// Same posture as every other producer in this platform.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishWorkflowStarted(ctx context.Context, w domain.WorkflowInstance) error {
	return p.emit("workflow.started", w.CorrelationID, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"tenant_id":            w.TenantID,
		"legal_entity_id":      w.LegalEntityID,
		"workflow_type":        w.WorkflowType,
		"initiated_by":         w.InitiatedBy,
		"started_at":           w.StartedAt,
	})
}

func (p *Publisher) PublishApprovalGranted(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage) error {
	return p.emit("approval.granted", w.CorrelationID, map[string]any{
		"workflow_instance_id":  w.WorkflowInstanceID,
		"stage_order":           stage.StageOrder,
		"approver_principal_id": stage.ApproverPrincipalID,
	})
}

func (p *Publisher) PublishApprovalRejected(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage) error {
	return p.emit("approval.rejected", w.CorrelationID, map[string]any{
		"workflow_instance_id":  w.WorkflowInstanceID,
		"stage_order":           stage.StageOrder,
		"approver_principal_id": stage.ApproverPrincipalID,
	})
}

func (p *Publisher) PublishWorkflowEscalated(ctx context.Context, w domain.WorkflowInstance) error {
	return p.emit("workflow.escalated", w.CorrelationID, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"current_stage":        w.CurrentStage,
	})
}

func (p *Publisher) PublishWorkflowCompleted(ctx context.Context, w domain.WorkflowInstance) error {
	return p.emit("workflow.completed", w.CorrelationID, map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"workflow_status":      w.WorkflowStatus,
		"completed_at":         w.CompletedAt,
	})
}

func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "workflow-svc",
		CorrelationID: correlationID,
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
	if err := p.producer.WriteMessages(context.Background(), msg); err != nil {
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
