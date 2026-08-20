package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.RiskScoreAssessment carries
// a real LegalEntityID but no jurisdiction field.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SourceService string      `json:"source_service"`
	SubjectID     string      `json:"subject_id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// PublishParams carries the envelope-level fields a call site supplies,
// alongside the payload-level business object.
type PublishParams struct {
	EventType     string
	SubjectID     string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher publishes events onto the Kafka event backbone.
//
// Before this fix, Publish had three real bugs: (1) it never set the
// event's id field at all — Event.ID had no assignment anywhere, so
// event_id was always the empty string; (2) it fired WriteMessages in a
// detached goroutine against context.Background(), decoupled from the
// caller entirely — no backpressure, no error visibility, and events
// could be lost if the process exited before the goroutine ran; (3) it
// unconditionally returned nil, so even a caller that checked the error
// would see success regardless of whether the write happened. All three
// are fixed: publish is synchronous, returns the real error, and stamps
// a fresh UUID per event.
type Publisher struct {
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

func NewPublisher(brokers []string, topic string, logger *zap.Logger) *Publisher {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}
	return &Publisher{writer: writer, topic: topic, logger: logger}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *Publisher {
	return &Publisher{writer: writer, topic: topic, logger: logger}
}

func (p *Publisher) Publish(ctx context.Context, params PublishParams) error {
	evt := Event{
		// A fresh UUID per publish, not the empty string it used to be —
		// see docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SourceService: "compliance-risk-scoring-svc",
		SubjectID:     params.SubjectID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}

	msg := kafka.Message{
		Key:   []byte(params.TenantID),
		Value: payload,
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Error("kafka publish failed — event dropped", zap.String("event_type", params.EventType), zap.Error(err))
		return err
	}

	p.logger.Info("event published", zap.String("event_type", params.EventType), zap.String("tenant_id", params.TenantID))
	return nil
}

func (p *Publisher) Close() error {
	if closer, ok := p.writer.(*kafka.Writer); ok && closer != nil {
		return closer.Close()
	}
	return nil
}
