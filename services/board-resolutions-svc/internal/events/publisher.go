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

type Event struct {
	EventID    string      `json:"event_id"`
	EventType  string      `json:"event_type"`
	EntityID   string      `json:"entity_id"`
	TenantID   string      `json:"tenant_id"`
	OccurredAt time.Time   `json:"occurred_at"`
	Payload    interface{} `json:"payload"`
}

type Publisher interface {
	Publish(ctx context.Context, eventType string, entityID string, tenantID string, payload interface{}) error
}

type KafkaPublisher struct {
	writer *kafka.Writer
	topic  string
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	var w *kafka.Writer
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

// Close flushes and releases the writer. Without it, messages sitting in the
// batch window at shutdown were simply lost.
func (p *KafkaPublisher) Close() error {
	if p.writer == nil {
		return nil
	}
	return p.writer.Close()
}

// Publish emits one domain event.
//
// It used to swallow the write error and return nil unconditionally, so the
// caller could not have reacted even if it had checked — and every caller
// discarded the result anyway. Now a failed publish is a returned error, and
// the handler logs it as a dropped event.
func (p *KafkaPublisher) Publish(ctx context.Context, eventType string, entityID string, tenantID string, payload interface{}) error {
	evt := Event{
		// The id used to be "evt-<type>-<entityID>", which is the same value
		// every time that entity emits that event — two vote tallies on one
		// resolution produced two events claiming to be the same event, so a
		// consumer deduplicating on event_id would drop the second.
		EventID:    "evt-" + uuid.New().String(),
		EventType:  eventType,
		EntityID:   entityID,
		TenantID:   tenantID,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if p.writer == nil {
		p.logger.Info("no kafka broker configured — event logged, not published",
			zap.String("event_type", eventType), zap.String("entity_id", entityID))
		return nil
	}
	if err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(entityID),
		Value: data,
	}); err != nil {
		return fmt.Errorf("kafka write %s: %w", eventType, err)
	}
	return nil
}
