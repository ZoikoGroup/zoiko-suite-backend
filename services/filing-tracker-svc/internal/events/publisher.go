package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// Unlike most services in this platform, filing-tracker-svc's own domain
// model (domain.FilingRequirement) actually carries a real JurisdictionID —
// this is one of the few places Jurisdiction is populated with real data
// rather than left empty for lack of a source.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	FilingID      string      `json:"filing_id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id,omitempty"`
	Jurisdiction  string      `json:"jurisdiction,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// PublishParams carries the envelope-level fields a call site supplies,
// alongside the payload-level business object.
type PublishParams struct {
	EventType     string
	FilingID      string
	TenantID      string
	LegalEntityID string
	Jurisdiction  string
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

// KafkaPublisher is the Kafka-backed implementation.
type KafkaPublisher struct {
	writer MessageWriter
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		BatchTimeout:           20 * time.Millisecond,
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: w, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, logger: logger}
}

// BuildEvent assembles the envelope for params with the given identity. The
// outbox writes it at transaction time, so it carries the actor and
// correlation of the request that caused the change.
func BuildEvent(params PublishParams, eventID string, occurredAt time.Time) Event {
	return Event{
		EventID:       eventID,
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "filing-tracker-svc",
		FilingID:      params.FilingID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		Jurisdiction:  params.Jurisdiction,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    occurredAt.UTC(),
		Payload:       params.Payload,
	}
}

// Publish sends a filing domain event to Kafka immediately. Request handlers
// no longer call it (they write to the transactional outbox, which the relay
// delivers through PublishOutbox); it is kept for tools and tests.
//
// EventID is a fresh UUID per call — it used to be the deterministic string
// "evt-"+eventType+"-"+filingID, which collides across every repeat
// occurrence of the same event type on the same filing. Any consumer
// deduplicating via INSERT ... ON CONFLICT (event_id) DO NOTHING — the
// exact pattern this platform's own evidence/history consumers use — would
// have silently dropped a second, real event as if it were a redelivery of
// the first.
func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	data, err := json.Marshal(BuildEvent(params, uuid.New().String(), time.Now()))
	if err != nil {
		return err
	}
	if err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(params.FilingID),
		Value: data,
	}); err != nil {
		p.logger.Warn("kafka publish failed",
			zap.String("event_type", params.EventType),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// PublishOutbox writes one already-serialised envelope from the outbox,
// keyed by filing ID as before, and returns the real delivery error so the
// relay retries instead of marking the row published.
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, _ string, aggregateID string, payload []byte) error {
	return p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(aggregateID), Value: payload})
}
