package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.BoardMeeting and
// domain.BoardResolution both carry real LegalEntityID and a
// CreatedBy/PassedBy actor; neither has a jurisdiction field.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
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
	EntityID      string
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
	topic  string
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	var w MessageWriter
	// An explicitly empty KAFKA_BROKERS means "no broker": publish in dry mode
	// rather than blocking every write on a dial to a broker that is not there.
	if len(brokers) > 0 {
		w = &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Topic:                  topic,
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
			// kafka-go's default is 1s, and WriteMessages is synchronous — so
			// every meeting, resolution, vote and pass paid a wasted second
			// inside the request, waiting out a batch window for a single
			// message. Platform-wide value, same as the other gap-closed
			// services.
			BatchTimeout: 10 * time.Millisecond,
		}
	}
	return &KafkaPublisher{writer: w, topic: topic, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

// Close flushes and releases the writer. Without it, messages sitting in the
// batch window at shutdown were simply lost.
func (p *KafkaPublisher) Close() error {
	if closer, ok := p.writer.(*kafka.Writer); ok && closer != nil {
		return closer.Close()
	}
	return nil
}

// Publish emits one domain event.
//
// It used to swallow the write error and return nil unconditionally, so the
// caller could not have reacted even if it had checked — and every caller
// discarded the result anyway. Now a failed publish is a returned error, and
// the handler logs it as a dropped event.
func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := Event{
		// The id used to be "evt-<type>-<entityID>", which is the same value
		// every time that entity emits that event — two vote tallies on one
		// resolution produced two events claiming to be the same event, so a
		// consumer deduplicating on event_id would drop the second.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SourceService: "board-resolutions-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if p.writer == nil {
		p.logger.Info("no kafka broker configured — event logged, not published",
			zap.String("event_type", params.EventType), zap.String("entity_id", params.EntityID))
		return nil
	}
	if err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(params.EntityID),
		Value: data,
	}); err != nil {
		return fmt.Errorf("kafka write %s: %w", params.EventType, err)
	}
	return nil
}
