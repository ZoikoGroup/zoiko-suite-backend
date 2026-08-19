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
// This service uses OrganizationID as its tenant-scoping concept (doc7's
// "organization is the top-level tenant object") — that value is what
// populates TenantID here. LegalEntityID/Jurisdiction are correctly
// omitted: none of CommercialAccount/Membership/CommercialSubscription/
// BillingSourceTransfer carry a legal_entity_id or jurisdiction field
// (Membership.LegalEntityID is a *different*, optional scoping concept
// specific to that one object, not populated here to avoid conflating it
// with the entity-scoping doctrine used elsewhere in this platform).
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
	TenantID      string      `json:"tenant_id"`
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
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: w, topic: topic, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := Event{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "commercial-account-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(params.EntityID),
		Value: data,
	})
	if err != nil {
		// Every direct handler call site discards this return value (`_ =
		// h.publisher.Publish(...)`), so returning the real error here
		// changes nothing for them — they keep behaving exactly as
		// before, fire-and-forget. It matters for internal/outbox's
		// Relay (via PublishForOutbox below), which actually checks this
		// error to decide whether to mark an outbox row published or
		// retry it; swallowing the error here would make that retry
		// logic dead code.
		p.logger.Warn("kafka publish failed", zap.String("event_type", params.EventType), zap.Error(err))
		return err
	}
	return nil
}

// PublishForOutbox adapts the outbox package's minimal, envelope-agnostic
// Publisher interface (internal/outbox deliberately has no import on this
// package, so it stays copyable to any other service unchanged) onto the
// real Publish above.
//
// ActorID/CorrelationID are empty here, not fabricated: outbox_events has
// no actor_id/correlation_id column, so a relayed event genuinely has
// neither to give — the row that fed this call only ever carried
// aggregate_type, aggregate_id, event_type, payload, and tenant_id.
// Extending the outbox schema to also capture actor/correlation at insert
// time (they ARE available in the handler that starts the transaction) is
// a real, separate follow-up — tracked in docs/architecture/known-gaps.md.
func (p *KafkaPublisher) PublishForOutbox(ctx context.Context, eventType, entityID, tenantID string, payload interface{}) error {
	return p.Publish(ctx, PublishParams{
		EventType: eventType,
		EntityID:  entityID,
		TenantID:  tenantID,
		Payload:   payload,
	})
}
