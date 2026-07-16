// Package consumer handles incoming workflow domain events from Kafka and
// persists them to the workflow history store as append-only evidence rows.
//
// Architectural constraints (doctrine.md, 03-microservices.md §3.7):
//
//   - Evidence services are read-only consumers of every other domain's events.
//     They NEVER mutate the source truth of any other service.
//   - Every consumer handler is idempotent: the store layer guarantees that
//     two deliveries of the same event_id produce exactly one stored row.
//   - Malformed events (missing required fields) are rejected and logged.
//     They are NOT stored partially and they do NOT crash the consumer loop.
//     Failure mode: fail-safe / degrade per event, not per consumer.
//   - For non-started events (approval.granted, approval.rejected,
//     workflow.escalated, workflow.completed) the payload does not carry
//     tenant_id or legal_entity_id (verified from workflow-svc/internal/events/
//     publisher.go). The consumer inherits these from the workflow.started row
//     already stored for the same workflow_instance_id via a single DB lookup.
//     If no started row exists yet (out-of-order delivery), the event is rejected
//     with a store error (not committed), so the broker re-delivers after
//     restart. This is the correct fail-closed posture for evidence integrity.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/store"
)

// envelope is the canonical event wrapper shared by all ZoikoSuite services
// (see workflow-svc/internal/events/publisher.go).
// Every event arriving on the Kafka topic is expected to conform to this shape.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     string          `json:"emitted_at"` // RFC3339 string from producer
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// workflowStartedPayload is the payload for the workflow.started event.
// This is the ONLY workflow event type that carries tenant_id and legal_entity_id.
//
// Shape (from workflow-svc/internal/events/publisher.go):
//
//	{
//	  "workflow_instance_id": "...",
//	  "tenant_id":            "...",
//	  "legal_entity_id":      "...",
//	  "workflow_type":        "...",
//	  "initiated_by":         "...",
//	  "started_at":           "..."
//	}
type workflowStartedPayload struct {
	WorkflowInstanceID string `json:"workflow_instance_id"`
	TenantID           string `json:"tenant_id"`
	LegalEntityID      string `json:"legal_entity_id"`
}

// workflowInstancePayload is the minimal shape shared by all non-started
// workflow events. They carry only workflow_instance_id; tenant/entity context
// is inherited from the workflow.started row.
//
// approval.granted / approval.rejected shape:
//
//	{ "workflow_instance_id": "...", "stage_order": ..., "approver_principal_id": "..." }
//
// workflow.escalated shape:
//
//	{ "workflow_instance_id": "...", "current_stage": ... }
//
// workflow.completed shape:
//
//	{ "workflow_instance_id": "...", "workflow_status": "...", "completed_at": "..." }
type workflowInstancePayload struct {
	WorkflowInstanceID string `json:"workflow_instance_id"`
}

// Consumer receives raw event messages (bytes), validates them, and delegates
// to the Store for append-only persistence.
type Consumer struct {
	appendStore store.AppendStore
	readStore   store.ReadStore
	log         *zap.Logger
}

// New returns a Consumer wired to the given stores and logger.
// Both AppendStore and ReadStore are required.
func New(appendStore store.AppendStore, readStore store.ReadStore, log *zap.Logger) *Consumer {
	return &Consumer{
		appendStore: appendStore,
		readStore:   readStore,
		log:         log,
	}
}

// Handle dispatches a raw Kafka message to the appropriate handler based on event_type.
//
// Contract:
//   - If the message cannot be parsed or the event_type is unknown, the message
//     is rejected (logged at error level) and nil is returned. The consumer
//     loop must NOT requeue unrecognised events — they would loop forever.
//   - If a required field is missing the message is rejected without partial storage.
//   - Duplicate event_ids (same eventID delivered twice) are handled atomically
//     by the store layer (INSERT … ON CONFLICT DO NOTHING).
//   - For non-started events, if no workflow.started row exists yet for the
//     workflow_instance_id (out-of-order delivery), a non-nil error is returned
//     so the caller does NOT commit the offset — the broker re-delivers.
//
// eventID is the globally unique event identifier supplied by the message broker.
// In the Kafka integration it comes from the X-Event-ID header; if absent, the
// runner provides a synthetic "topic:partition:offset" fallback.
func (c *Consumer) Handle(ctx context.Context, eventID string, raw []byte) error {
	if eventID == "" {
		c.log.Error("rejected: event_id is empty",
			zap.ByteString("raw_message", raw),
		)
		return nil // non-fatal: do not crash the consumer loop
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("rejected: cannot unmarshal event envelope",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	if env.EventType == "" {
		c.log.Error("rejected: event_type is missing",
			zap.String("event_id", eventID),
		)
		return nil
	}
	if env.SourceService == "" {
		c.log.Error("rejected: source_service is missing",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}
	if env.SchemaVersion == "" {
		c.log.Error("rejected: schema_version is missing",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}

	switch env.EventType {
	case "workflow.started":
		return c.handleWorkflowStarted(ctx, eventID, env)
	case "approval.granted", "approval.rejected", "workflow.escalated", "workflow.completed":
		return c.handleWorkflowTransition(ctx, eventID, env)
	default:
		c.log.Warn("unknown event_type — skipped",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}
}

// handleWorkflowStarted processes the workflow.started event.
//
// Required payload fields: workflow_instance_id, tenant_id, legal_entity_id.
// This is the only event type that provides tenant/entity context.
func (c *Consumer) handleWorkflowStarted(ctx context.Context, eventID string, env envelope) error {
	var p workflowStartedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.log.Error("rejected: cannot unmarshal workflow.started payload",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	if p.WorkflowInstanceID == "" {
		c.log.Error("rejected: workflow.started missing workflow_instance_id",
			zap.String("event_id", eventID),
		)
		return nil
	}
	if p.TenantID == "" {
		c.log.Error("rejected: workflow.started missing tenant_id",
			zap.String("event_id", eventID),
		)
		return nil
	}
	if p.LegalEntityID == "" {
		c.log.Error("rejected: workflow.started missing legal_entity_id",
			zap.String("event_id", eventID),
		)
		return nil
	}

	evt := store.WorkflowHistoryEvent{
		EventID:            eventID,
		WorkflowInstanceID: p.WorkflowInstanceID,
		EventType:          env.EventType,
		CorrelationID:      env.CorrelationID,
		TenantID:           p.TenantID,
		LegalEntityID:      p.LegalEntityID,
		Payload:            env.Payload,
	}

	if err := c.appendStore.Append(ctx, evt); err != nil {
		return fmt.Errorf("handleWorkflowStarted: append: %w", err)
	}

	c.log.Info("workflow.started stored",
		zap.String("event_id", eventID),
		zap.String("workflow_instance_id", p.WorkflowInstanceID),
		zap.String("tenant_id", p.TenantID),
		zap.String("correlation_id", env.CorrelationID),
	)
	return nil
}

// handleWorkflowTransition processes approval.granted, approval.rejected,
// workflow.escalated, and workflow.completed events.
//
// These event types carry only workflow_instance_id in their payload;
// tenant_id and legal_entity_id are inherited from the workflow.started row.
// If no started row exists yet (out-of-order delivery), a non-nil error is
// returned so the broker re-delivers after restart.
func (c *Consumer) handleWorkflowTransition(ctx context.Context, eventID string, env envelope) error {
	var p workflowInstancePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.log.Error("rejected: cannot unmarshal transition event payload",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
			zap.Error(err),
		)
		return nil
	}

	if p.WorkflowInstanceID == "" {
		c.log.Error("rejected: transition event missing workflow_instance_id",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}

	// Inherit tenant context from the workflow.started row.
	// Fail-closed: if no started row exists, the event is not committed.
	tc, found, err := c.readStore.GetTenantContext(ctx, p.WorkflowInstanceID)
	if err != nil {
		return fmt.Errorf("handleWorkflowTransition: get tenant context for %q: %w",
			p.WorkflowInstanceID, err)
	}
	if !found {
		// Out-of-order delivery or the started event was not yet persisted.
		// Return a non-nil error so the runner does NOT commit the offset —
		// the broker will re-deliver after restart.
		return fmt.Errorf("handleWorkflowTransition: no tenant context found for workflow_instance_id %q"+
			" (workflow.started event not yet persisted — will retry)",
			p.WorkflowInstanceID)
	}

	evt := store.WorkflowHistoryEvent{
		EventID:            eventID,
		WorkflowInstanceID: p.WorkflowInstanceID,
		EventType:          env.EventType,
		CorrelationID:      env.CorrelationID,
		TenantID:           tc.TenantID,
		LegalEntityID:      tc.LegalEntityID,
		Payload:            env.Payload,
	}

	if err := c.appendStore.Append(ctx, evt); err != nil {
		return fmt.Errorf("handleWorkflowTransition: append: %w", err)
	}

	c.log.Info("workflow transition stored",
		zap.String("event_id", eventID),
		zap.String("event_type", env.EventType),
		zap.String("workflow_instance_id", p.WorkflowInstanceID),
		zap.String("tenant_id", tc.TenantID),
		zap.String("correlation_id", env.CorrelationID),
	)
	return nil
}
