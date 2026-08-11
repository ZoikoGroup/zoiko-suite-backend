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

// PublishReviewCreated fires when a review record is first created against
// a validated employee, inside an open cycle.
func (p *Publisher) PublishReviewCreated(ctx context.Context, r domain.ReviewRecord) {
	p.emit(ctx, "review.created", r.CorrelationID, r.ReviewID, map[string]any{
		"review_id":       r.ReviewID,
		"legal_entity_id": r.LegalEntityID,
		"cycle_id":        r.CycleID,
		"employee_id":     r.EmployeeID,
	})
}

// PublishReviewCompleted fires once a submitted review is finalized.
func (p *Publisher) PublishReviewCompleted(ctx context.Context, r domain.ReviewRecord) {
	p.emit(ctx, "review.completed", r.CorrelationID, r.ReviewID, map[string]any{
		"review_id":       r.ReviewID,
		"legal_entity_id": r.LegalEntityID,
		"cycle_id":        r.CycleID,
		"employee_id":     r.EmployeeID,
		"rating":          r.Rating,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
