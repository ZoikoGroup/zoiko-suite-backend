// Package events defines this service's event envelope and the Kafka writer the
// outbox relay uses to deliver it.
//
// Handlers never publish directly: they write the event into the outbox table
// inside the business transaction (internal/outbox), and the relay later calls
// PublishRaw with the exact bytes that were committed. Publish remains for
// callers and tests that need a one-shot synchronous publish.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event names emitted by this service.
//
// FIN-02 forecast events use the Enterprise Event Catalogue's
// "finance.forecast.<verb>" form (the catalogue lists finance.forecast.published).
// AI-05 events use the exact names given by the AI-05 specification.
const (
	EventForecastCreated    = "finance.forecast.created"
	EventForecastCalculated = "finance.forecast.calculated"
	EventForecastSubmitted  = "finance.forecast.submitted"
	EventForecastApproved   = "finance.forecast.approved"
	EventForecastPublished  = "finance.forecast.published"
	EventForecastSuperseded = "finance.forecast.superseded"
	EventForecastArchived   = "finance.forecast.archived"

	EventForecastSuggestionGenerated = "AI.ForecastSuggestionGenerated"
	EventForecastSuggestionAccepted  = "AI.ForecastSuggestionAccepted"
)

// Event is this platform's event contract (Doc 03 §19): event name, version,
// timestamp, tenant, legal entity, actor, correlation, source service and
// payload.
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

// BuildEvent stamps a fresh event_id and timestamp onto params. The event_id is
// allocated here, before the outbox row is committed, so a retried publish
// always carries the same id (Event Catalogue §6.1).
func BuildEvent(params PublishParams) Event {
	return Event{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SourceService: "forecasting-svc",
		SubjectID:     params.SubjectID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
}

// MessageWriter is the one method Publisher needs from *kafka.Writer, narrowed
// so tests can substitute a fake broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes events to Kafka synchronously and returns the real error.
type Publisher struct {
	writer MessageWriter
	logger *zap.Logger
}

func NewPublisher(brokers []string, topic string, logger *zap.Logger) *Publisher {
	writer := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		WriteTimeout:           5 * time.Second,
	}
	return &Publisher{writer: writer, logger: logger}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied writer (tests).
func NewPublisherWithWriter(writer MessageWriter, logger *zap.Logger) *Publisher {
	return &Publisher{writer: writer, logger: logger}
}

// Publish builds and synchronously writes one event.
func (p *Publisher) Publish(ctx context.Context, params PublishParams) error {
	payload, err := json.Marshal(BuildEvent(params))
	if err != nil {
		return err
	}
	return p.PublishRaw(ctx, params.TenantID, payload)
}

// PublishRaw writes already-serialised event bytes, keyed by tenant so one
// tenant's events stay ordered within a partition.
func (p *Publisher) PublishRaw(ctx context.Context, partitionKey string, value []byte) error {
	msg := kafka.Message{Key: []byte(partitionKey), Value: value}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Warn("kafka publish failed", zap.Error(err))
		return err
	}
	return nil
}

func (p *Publisher) Close() error {
	if closer, ok := p.writer.(*kafka.Writer); ok && closer != nil {
		return closer.Close()
	}
	return nil
}
