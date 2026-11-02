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
// service, and payload schema version. domain.AnomalyRecord carries a
// real LegalEntityID but no jurisdiction field.
type Event struct {
	EventID      string `json:"event_id"`
	EventType    string `json:"event_type"`
	EventVersion string `json:"event_version"`
	// SchemaVersion is the field the platform audit consumer (audit-event-store-svc)
	// requires; EventVersion is kept for existing consumers. Same value.
	SchemaVersion string      `json:"schema_version"`
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
	// EventID and OccurredAt are set by the outbox relay so a redelivered
	// event keeps its original identity; when empty, Publish generates them.
	EventID       string
	OccurredAt    time.Time
	EventType     string
	SubjectID     string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// Publisher is the interface for emitting domain events.
type Publisher interface {
	Publish(ctx context.Context, params PublishParams) error
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type KafkaPublisher struct {
	writer MessageWriter
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
		// The outbox relay publishes one event at a time; the 1s default
		// batch timeout would cap it at one event per second.
		BatchTimeout:           10 * time.Millisecond,
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: writer, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, logger: logger}
}

func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	eventID := params.EventID
	if eventID == "" {
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		eventID = "evt-" + uuid.New().String()
	}
	occurredAt := params.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	evt := Event{
		EventID:       eventID,
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "anomaly-detection-svc",
		SubjectID:     params.SubjectID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    occurredAt,
		Payload:       params.Payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}

	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(params.SubjectID),
		Value: data,
	})
	if err != nil {
		p.logger.Error("failed to publish Kafka event", zap.String("event_type", params.EventType), zap.Error(err))
		return err
	}
	p.logger.Info("published Kafka event", zap.String("event_type", params.EventType), zap.String("subject_id", params.SubjectID))
	return nil
}
