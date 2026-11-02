// Package events defines this service's event envelope and the Kafka writer
// the outbox relay publishes through.
//
// Handlers never publish directly: they enqueue an envelope into the
// transactional outbox (store.EnqueueEvent) inside the same transaction as
// the state change, and internal/outbox.Relay delivers it. That way an event
// is published if and only if its state change committed.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// SourceService is stamped on every event.
const SourceService = "compliance-risk-scoring-svc"

// Event types emitted by this service. See ROUTES.md "Events".
const (
	TypeRiskScoreCalculated     = "risk_score.calculated"
	TypeRiskThresholdExceeded   = "risk_threshold.exceeded"
	TypeRiskScoreSuperseded     = "risk_score.superseded"
	TypeRiskScoreReviewed       = "risk_score.reviewed"
	TypeRiskAssessmentArchived  = "risk_assessment.archived"
	TypeThresholdRuleCreated    = "risk_threshold_rule.created"
	TypeThresholdRuleUpdated    = "risk_threshold_rule.updated"
	TypeThresholdRuleApproved   = "risk_threshold_rule.approved"
	TypeThresholdRuleSuperseded = "risk_threshold_rule.superseded"
	TypeThresholdRuleRetired    = "risk_threshold_rule.retired"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event carries event name, version, timestamp, tenant, legal entity, actor,
// correlation ID, source service and payload. Jurisdiction is not carried:
// a risk score has no jurisdiction of its own (see ROUTES.md gaps).
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
	RequestID     string      `json:"request_id,omitempty"`
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
	RequestID     string
	Payload       interface{}
}

// BuildEvent assembles the envelope for params with the given identity.
func BuildEvent(params PublishParams, eventID string, occurredAt time.Time) Event {
	return Event{
		EventID:       eventID,
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: SourceService,
		SubjectID:     params.SubjectID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		RequestID:     params.RequestID,
		OccurredAt:    occurredAt.UTC(),
		Payload:       params.Payload,
	}
}

// MessageWriter is the one method Publisher needs from *kafka.Writer,
// narrowed so tests can substitute a fake.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes envelopes to Kafka synchronously and returns the real
// error, so the outbox relay knows whether to mark an event delivered.
type Publisher struct {
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

// NewPublisher builds a Kafka-backed publisher. BatchTimeout is lowered from
// kafka-go's 1s default because the relay writes small synchronous batches.
func NewPublisher(brokers []string, topic string, logger *zap.Logger) *Publisher {
	writer := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		BatchTimeout:           20 * time.Millisecond,
		RequiredAcks:           kafka.RequireOne,
		AllowAutoTopicCreation: true,
	}
	return &Publisher{writer: writer, topic: topic, logger: logger}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied writer.
func NewPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *Publisher {
	return &Publisher{writer: writer, topic: topic, logger: logger}
}

// WriteEnvelope publishes an already-serialised envelope. The tenant ID is
// the partition key so one tenant's events stay ordered.
func (p *Publisher) WriteEnvelope(ctx context.Context, tenantID string, envelope []byte) error {
	return p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(tenantID), Value: envelope})
}

// Publish builds and writes one event immediately. Retained for tools and
// tests; request handlers use the outbox instead.
func (p *Publisher) Publish(ctx context.Context, params PublishParams) error {
	event := BuildEvent(params, "evt-"+uuid.New().String(), time.Now())
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := p.WriteEnvelope(ctx, params.TenantID, payload); err != nil {
		p.logger.Error("kafka publish failed", zap.String("event_type", params.EventType), zap.Error(err))
		return err
	}
	return nil
}

// Close releases the Kafka writer.
func (p *Publisher) Close() error {
	if closer, ok := p.writer.(*kafka.Writer); ok && closer != nil {
		return closer.Close()
	}
	return nil
}
