package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/offboarding-severance-svc/internal/domain"
)

// EventEnvelope is this platform's event contract (Doc 03 §19): every
// published event must carry event name, event version, timestamp,
// tenant ID, legal entity ID, jurisdiction context, actor ID, correlation
// ID, source service, and payload schema version. domain.
// TerminationRequest and domain.OffboardingChecklist both carry real
// TenantID/LegalEntityID/CorrelationID; neither has a jurisdiction
// field. actor_id is the caller-supplied principalID — previously
// accepted by every PublishX method but silently discarded, never
// reaching the envelope at all.
type EventEnvelope struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	EventVersion  string    `json:"event_version"`
	SourceService string    `json:"source_service"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	ActorID       string    `json:"actor_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	Payload       any       `json:"payload"`
}

type Publisher interface {
	PublishTerminationInitiated(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishTerminationApproved(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishEmployeeTerminated(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishOffboardingCompleted(ctx context.Context, principalID string, chk domain.OffboardingChecklist)
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type KafkaPublisher struct {
	writer MessageWriter
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	if len(brokers) == 0 || brokers[0] == "" {
		return &KafkaPublisher{logger: logger}
	}
	w := &kafka.Writer{
		Addr:     kafka.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}
	return &KafkaPublisher{writer: w, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, logger: logger}
}

func (p *KafkaPublisher) publish(ctx context.Context, eventType, tenantID, legalEntityID, actorID, correlationID string, payload any) {
	env := EventEnvelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		SourceService: "offboarding-severance-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Timestamp:     time.Now().UTC(),
		Payload:       payload,
	}

	data, err := json.Marshal(env)
	if err != nil {
		p.logger.Error("failed to marshal event envelope", zap.Error(err))
		return
	}

	if p.writer == nil {
		p.logger.Info("stub event publish", zap.String("event_type", eventType), zap.String("tenant_id", tenantID))
		return
	}

	msg := kafka.Message{
		Key:   []byte(tenantID),
		Value: data,
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Error("failed to publish kafka event", zap.String("event_type", eventType), zap.Error(err))
	} else {
		p.logger.Info("kafka event published", zap.String("event_type", eventType), zap.String("event_id", env.EventID))
	}
}

func (p *KafkaPublisher) PublishTerminationInitiated(ctx context.Context, principalID string, req domain.TerminationRequest) {
	p.publish(ctx, "termination.initiated", req.TenantID, req.LegalEntityID, principalID, req.CorrelationID, req)
}

func (p *KafkaPublisher) PublishTerminationApproved(ctx context.Context, principalID string, req domain.TerminationRequest) {
	p.publish(ctx, "termination.approved", req.TenantID, req.LegalEntityID, principalID, req.CorrelationID, req)
}

func (p *KafkaPublisher) PublishEmployeeTerminated(ctx context.Context, principalID string, req domain.TerminationRequest) {
	p.publish(ctx, "employee.terminated", req.TenantID, req.LegalEntityID, principalID, req.CorrelationID, req)
}

func (p *KafkaPublisher) PublishOffboardingCompleted(ctx context.Context, principalID string, chk domain.OffboardingChecklist) {
	p.publish(ctx, "offboarding.completed", chk.TenantID, chk.LegalEntityID, principalID, chk.CorrelationID, chk)
}
