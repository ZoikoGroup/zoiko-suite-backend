package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/performance-review-svc/internal/domain"
)

// envelope is the canonical ZoikoSuite event wrapper used across all services.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Publisher publishes domain events to the zoiko.workforce.events Kafka topic.
// Publish failures are logged but never block the calling handler — events
// are fire-and-forget consistent with platform doctrine (source truth is DB, not Kafka).
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishReviewCycleCreated emits review.cycle.created when a new cycle is opened.
func (p *Publisher) PublishReviewCycleCreated(ctx context.Context, correlationID string, c domain.ReviewCycle) {
	p.emit(ctx, "review.cycle.created", correlationID, map[string]any{
		"review_cycle_id": c.ReviewCycleID,
		"tenant_id":       c.TenantID,
		"legal_entity_id": c.LegalEntityID,
		"cycle_name":      c.CycleName,
		"cycle_type":      c.CycleType,
		"start_date":      c.StartDate,
		"end_date":        c.EndDate,
		"cycle_status":    c.CycleStatus,
		"created_at":      c.CreatedAt,
	})
}

// PublishReviewCreated emits review.created when a review is initiated for an employee.
func (p *Publisher) PublishReviewCreated(ctx context.Context, correlationID string, r domain.PerformanceReview) {
	p.emit(ctx, "review.created", correlationID, map[string]any{
		"performance_review_id": r.PerformanceReviewID,
		"tenant_id":             r.TenantID,
		"legal_entity_id":       r.LegalEntityID,
		"review_cycle_id":       r.ReviewCycleID,
		"employee_id":           r.EmployeeID,
		"reviewer_principal_id": r.ReviewerPrincipalID,
		"review_status":         r.ReviewStatus,
		"created_at":            r.CreatedAt,
	})
}

// PublishReviewCompleted emits review.completed when a review reaches the COMPLETED
// terminal state. Consumed by compensation-svc to trigger merit/bonus workflows.
func (p *Publisher) PublishReviewCompleted(ctx context.Context, correlationID string, r domain.PerformanceReview) {
	p.emit(ctx, "review.completed", correlationID, map[string]any{
		"performance_review_id": r.PerformanceReviewID,
		"tenant_id":             r.TenantID,
		"legal_entity_id":       r.LegalEntityID,
		"review_cycle_id":       r.ReviewCycleID,
		"employee_id":           r.EmployeeID,
		"reviewer_principal_id": r.ReviewerPrincipalID,
		"overall_rating":        r.OverallRating,
		"governance_decision_id": r.GovernanceDecisionID,
		"completed_at":          r.CompletedAt,
	})
}

// emit marshals the payload into the canonical envelope and writes to Kafka.
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
		SourceService: "performance-review-svc",
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
