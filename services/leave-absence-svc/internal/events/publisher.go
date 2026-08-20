package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/leave-absence-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. Neither domain.LeaveRequest nor
// domain.LeaveBalance has a legal_entity_id or jurisdiction field of its
// own — the employee's legal entity is resolved at each handler call site
// (via resolveEmployeeEntity) and threaded through explicitly, alongside
// the already-verified principalID as actor_id.
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

func (p *Publisher) PublishLeaveRequested(ctx context.Context, correlationID, legalEntityID, actorID string, r domain.LeaveRequest) {
	p.emit(ctx, "leave.requested", correlationID, r.TenantID, legalEntityID, actorID, r.RequestID, map[string]any{
		"request_id":    r.RequestID,
		"tenant_id":     r.TenantID,
		"employee_id":   r.EmployeeID,
		"leave_type_id": r.LeaveTypeID,
		"start_date":    r.StartDate,
		"end_date":      r.EndDate,
		"total_hours":   r.TotalHours,
		"submitted_at":  r.CreatedAt,
	})
}

func (p *Publisher) PublishLeaveApproved(ctx context.Context, correlationID, legalEntityID string, r domain.LeaveRequest) {
	actorID := ""
	if r.ReviewerID != nil {
		actorID = *r.ReviewerID
	}
	p.emit(ctx, "leave.approved", correlationID, r.TenantID, legalEntityID, actorID, r.RequestID, map[string]any{
		"request_id":     r.RequestID,
		"tenant_id":      r.TenantID,
		"employee_id":    r.EmployeeID,
		"leave_type_id":  r.LeaveTypeID,
		"total_hours":    r.TotalHours,
		"reviewer_id":    r.ReviewerID,
		"reviewer_notes": r.ReviewerNotes,
		"approved_at":    r.ReviewedAt,
	})
}

func (p *Publisher) PublishLeaveRejected(ctx context.Context, correlationID, legalEntityID string, r domain.LeaveRequest) {
	actorID := ""
	if r.ReviewerID != nil {
		actorID = *r.ReviewerID
	}
	p.emit(ctx, "leave.rejected", correlationID, r.TenantID, legalEntityID, actorID, r.RequestID, map[string]any{
		"request_id":     r.RequestID,
		"tenant_id":      r.TenantID,
		"employee_id":    r.EmployeeID,
		"reviewer_id":    r.ReviewerID,
		"reviewer_notes": r.ReviewerNotes,
		"rejected_at":    r.ReviewedAt,
	})
}

func (p *Publisher) PublishBalanceUpdated(ctx context.Context, correlationID, legalEntityID, actorID string, b domain.LeaveBalance) {
	p.emit(ctx, "leave.balance.updated", correlationID, b.TenantID, legalEntityID, actorID, b.BalanceID, map[string]any{
		"balance_id":      b.BalanceID,
		"tenant_id":       b.TenantID,
		"employee_id":     b.EmployeeID,
		"leave_type_id":   b.LeaveTypeID,
		"allocated_hours": b.AllocatedHours,
		"used_hours":      b.UsedHours,
		"pending_hours":   b.PendingHours,
		"available_hours": b.AvailableHours,
		"updated_at":      b.UpdatedAt,
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
		SourceService: "leave-absence-svc",
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
