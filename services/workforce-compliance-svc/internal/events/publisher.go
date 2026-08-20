package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/workforce-compliance-svc/internal/domain"
)

// EventEnvelope is this platform's event contract (Doc 03 §19): every
// published event must carry event id, event type, event version,
// timestamp, tenant id, legal entity id, actor id, correlation id, source
// service, and payload. No jurisdiction field exists anywhere in this
// service's domain objects, so it is omitted here rather than fabricated.
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

// Publisher's principalID parameters below are the caller's already
// authz-checked identity (from handler.go's X-Principal-Id) and are the
// event's actor_id — ComplianceAlert carries no correlation_id of its own,
// so PublishComplianceAlertRaised takes one explicitly from whichever
// record (visa or hours log) triggered the alert.
type Publisher interface {
	PublishWorkAuthVerified(ctx context.Context, principalID string, auth domain.WorkAuthorization)
	PublishVisaExpirationFlagged(ctx context.Context, principalID string, visa domain.VisaRecord)
	PublishWorkingHoursBreach(ctx context.Context, principalID string, log domain.WorkingHourLog)
	PublishComplianceAlertRaised(ctx context.Context, principalID string, correlationID string, alert domain.ComplianceAlert)
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
	if len(brokers) == 0 || brokers[0] == "" {
		return &KafkaPublisher{logger: logger}
	}
	w := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: topic, Balancer: &kafka.LeastBytes{}}
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
		SourceService: "workforce-compliance-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Timestamp:     time.Now().UTC(),
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		p.logger.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if p.writer == nil {
		p.logger.Info("stub event publish", zap.String("event_type", eventType), zap.String("tenant_id", tenantID))
		return
	}
	msg := kafka.Message{Key: []byte(tenantID), Value: data}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Error("failed to publish kafka event", zap.String("event_type", eventType), zap.Error(err))
	} else {
		p.logger.Info("kafka event published", zap.String("event_type", eventType), zap.String("event_id", env.EventID))
	}
}

func (p *KafkaPublisher) PublishWorkAuthVerified(ctx context.Context, principalID string, auth domain.WorkAuthorization) {
	p.publish(ctx, "work_authorization.verified", auth.TenantID, auth.LegalEntityID, principalID, auth.CorrelationID, auth)
}

func (p *KafkaPublisher) PublishVisaExpirationFlagged(ctx context.Context, principalID string, visa domain.VisaRecord) {
	p.publish(ctx, "visa.expiration_flagged", visa.TenantID, visa.LegalEntityID, principalID, visa.CorrelationID, visa)
}

func (p *KafkaPublisher) PublishWorkingHoursBreach(ctx context.Context, principalID string, log domain.WorkingHourLog) {
	p.publish(ctx, "working_hours.breach_detected", log.TenantID, log.LegalEntityID, principalID, log.CorrelationID, log)
}

func (p *KafkaPublisher) PublishComplianceAlertRaised(ctx context.Context, principalID string, correlationID string, alert domain.ComplianceAlert) {
	p.publish(ctx, "compliance.alert_raised", alert.TenantID, alert.LegalEntityID, principalID, correlationID, alert)
}
