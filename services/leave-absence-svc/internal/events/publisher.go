package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/leave-absence-svc/internal/domain"
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

func (p *Publisher) PublishLeaveRequested(ctx context.Context, correlationID string, r domain.LeaveRequest) {
	p.emit(ctx, "leave.requested", correlationID, map[string]any{
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

func (p *Publisher) PublishLeaveApproved(ctx context.Context, correlationID string, r domain.LeaveRequest) {
	p.emit(ctx, "leave.approved", correlationID, map[string]any{
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

func (p *Publisher) PublishLeaveRejected(ctx context.Context, correlationID string, r domain.LeaveRequest) {
	p.emit(ctx, "leave.rejected", correlationID, map[string]any{
		"request_id":     r.RequestID,
		"tenant_id":      r.TenantID,
		"employee_id":    r.EmployeeID,
		"reviewer_id":    r.ReviewerID,
		"reviewer_notes": r.ReviewerNotes,
		"rejected_at":    r.ReviewedAt,
	})
}

func (p *Publisher) PublishBalanceUpdated(ctx context.Context, correlationID string, b domain.LeaveBalance) {
	p.emit(ctx, "leave.balance.updated", correlationID, map[string]any{
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
		SourceService: "leave-absence-svc",
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