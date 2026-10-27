// Package events carries privacy-purpose-registry-svc's event envelope and its
// ONLY delivery path: the transactional outbox (outbox.go).
//
// There is deliberately no "publish after commit" API. A handler that
// published directly, in addition to the store enqueuing into the outbox,
// emitted every lifecycle event twice (two different event IDs, so consumers
// could not de-duplicate) — and the direct copy was lost on any broker outage.
// State changes now enqueue inside their own transaction (EnqueueTx) and the
// relay is the sole thing that talks to Kafka.
package events

import (
	"context"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// Event is this platform's event contract (Doc 03 §19 / ZS-EVENT-001's
// predecessor — see docs/architecture/master-register-findings for the
// pending estate-wide envelope migration, not undertaken by this service).
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
	TenantID      string      `json:"tenant_id,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// PublishParams carries the envelope-level fields a call site supplies to
// EnqueueTx.
type PublishParams struct {
	EventType     string
	EntityID      string
	TenantID      string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// MessageWriter is the one method the relay needs from *kafka.Writer —
// narrowed so tests can substitute a fake without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// NewKafkaWriter builds the writer the relay publishes through.
//
// RequiredAcks is RequireAll on purpose: kafka-go's default is
// fire-and-forget, which would let the relay stamp an event published_at
// before any broker had stored it — reintroducing, one layer down, exactly
// the loss the outbox exists to prevent.
func NewKafkaWriter(brokers []string, topic string) *kafka.Writer {
	return &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		RequiredAcks:           kafka.RequireAll,
	}
}
