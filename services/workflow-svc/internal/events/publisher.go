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
	payload := map[string]any{
		"workflow_instance_id": w.WorkflowInstanceID,
		"tenant_id":            w.TenantID,
		"legal_entity_id":      w.LegalEntityID,
		"workflow_type":        w.WorkflowType,
		"initiated_by":         w.InitiatedBy,
		"started_at":           w.StartedAt,
	}
	if w.SubjectType != nil {
		payload["subject_type"] = *w.SubjectType
	}
	if w.SubjectID != nil {
		payload["subject_id"] = *w.SubjectID
	}
	if w.SubjectVersion != nil {
		payload["subject_version"] = *w.SubjectVersion
	}
	if w.SubjectFingerprint != nil {
		payload["subject_fingerprint"] = *w.SubjectFingerprint
	}
	return p.emit(ctx, "workflow.started", w.CorrelationID, w.TenantID, w.LegalEntityID, w.InitiatedBy, payload)
}

// PublishWorkflowInvalidated emits workflow.approval.invalidated when an approval request
// is invalidated due to a material subject change or policy invalidation per ZS-STATE-001 §6.1 / §7.
func (p *Publisher) PublishWorkflowInvalidated(ctx context.Context, w domain.WorkflowInstance, actorID string) error {
	payload := map[string]any{
		"workflow_instance_id":     w.WorkflowInstanceID,
		"workflow_status":          w.WorkflowStatus,
		"invalidated_at":           w.InvalidatedAt,
		"invalidation_reason_code": w.InvalidationReasonCode,
	}
	if w.SubjectType != nil {
		payload["subject_type"] = *w.SubjectType
	}
	if w.SubjectID != nil {
		payload["subject_id"] = *w.SubjectID
	}
	if w.SubjectVersion != nil {
		payload["subject_version"] = *w.SubjectVersion
	}
	if w.SubjectFingerprint != nil {
		payload["subject_fingerprint"] = *w.SubjectFingerprint
	}
	if w.InvalidationNarrative != nil {
		payload["invalidation_narrative"] = *w.InvalidationNarrative
	}
	if len(w.InvalidationEvidenceRefs) > 0 {
		payload["invalidation_evidence_refs"] = w.InvalidationEvidenceRefs
	}
	return p.emit(ctx, "workflow.approval.invalidated", w.CorrelationID, w.TenantID, w.LegalEntityID, actorID, payload)
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

// PublishAuditEngagementEvent publishes only an allow-listed AUD-01 lifecycle
// fact. The audit-event-store consumer persists these events in its immutable
// hash chain; this service remains the sole owner of engagement state.
func (p *Publisher) PublishAuditEngagementEvent(ctx context.Context, eventType string, e domain.AuditEngagement, actorID, correlationID string) error {
	switch eventType {
	case "audit.engagement.created", "audit.engagement.acceptance_submitted", "audit.engagement.accepted", "audit.engagement.rejected", "audit.engagement.activated", "audit.engagement.withdrawn",
		"audit.engagement.scope_amended", "audit.engagement.fieldwork_complete", "audit.engagement.completion_review_entered", "audit.engagement.report_ready", "audit.engagement.closed":
	default:
		return fmt.Errorf("audit engagement: unsupported event type %q", eventType)
	}
	return p.emit(ctx, eventType, correlationID, e.TenantID, e.LegalEntityID, actorID, map[string]any{
		"engagement_id":               e.EngagementID,
		"tenant_id":                   e.TenantID,
		"legal_entity_id":             e.LegalEntityID,
		"engagement_code":             e.EngagementCode,
		"engagement_type":             e.EngagementType,
		"status":                      e.Status,
		"actor_principal_id":          actorID,
		"reporting_period_start":      e.ReportingPeriodStart,
		"reporting_period_end":        e.ReportingPeriodEnd,
		"framework_profile_id":        e.FrameworkProfileID,
		"framework_profile_version":   e.FrameworkProfileVersion,
		"methodology_id":              e.MethodologyID,
		"methodology_version":         e.MethodologyVersion,
		"acceptance_document_id":      e.AcceptanceDocumentID,
		"acceptance_document_version": e.AcceptanceDocumentVersion,
	})
}

// PublishFormEvent backs BIZ-04's submission events — SubmissionReceived,
// SubmissionValidated, SubmissionRejected, SubmissionRouted — same
// generic-per-domain-emitter shape as PublishAuditEngagementEvent above.
// FormPublished is a FormDefinition event, published separately via
// PublishFormPublished below.
func (p *Publisher) PublishFormEvent(ctx context.Context, eventType string, s domain.FormSubmission, actorID, correlationID string) error {
	switch eventType {
	case "form.submission.received", "form.submission.validated", "form.submission.rejected", "form.submission.routed":
	default:
		return fmt.Errorf("form: unsupported event type %q", eventType)
	}
	return p.emit(ctx, eventType, correlationID, s.TenantID, s.LegalEntityID, actorID, map[string]any{
		"submission_id":      s.SubmissionID,
		"form_id":            s.FormID,
		"tenant_id":          s.TenantID,
		"legal_entity_id":    s.LegalEntityID,
		"form_version":       s.FormVersion,
		"status":             s.Status,
		"actor_principal_id": actorID,
	})
}

// PublishFormPublished backs the one BIZ-04 event that has no
// FormSubmission to source — FormPublished is a FormDefinition event.
func (p *Publisher) PublishFormPublished(ctx context.Context, f domain.FormDefinition, actorID, correlationID string) error {
	return p.emit(ctx, "form.published", correlationID, f.TenantID, f.LegalEntityID, actorID, map[string]any{
		"form_id":            f.FormID,
		"tenant_id":          f.TenantID,
		"legal_entity_id":    f.LegalEntityID,
		"name":               f.Name,
		"target_domain":      f.TargetDomain,
		"version":            f.Version,
		"actor_principal_id": actorID,
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

// PublishOutbox publishes an event from the transactional outbox relay, preserving
// the stable outboxEventID as the X-Event-ID Kafka header across all retries.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, eventType, correlationID, tenantID, legalEntityID, actorID string, payload []byte) error {
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
		Payload:       json.RawMessage(payload),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{
		Value: data,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(outboxEventID)},
		},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("outbox event published",
		zap.String("event_id", outboxEventID),
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
	return nil
}
