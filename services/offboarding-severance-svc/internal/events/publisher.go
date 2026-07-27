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

type EventEnvelope struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Timestamp     time.Time `json:"timestamp"`
	Payload       any       `json:"payload"`
}

type Publisher interface {
	PublishTerminationInitiated(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishTerminationApproved(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishEmployeeTerminated(ctx context.Context, principalID string, req domain.TerminationRequest)
	PublishOffboardingCompleted(ctx context.Context, principalID string, chk domain.OffboardingChecklist)
}

type KafkaPublisher struct {
	writer *kafka.Writer
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

func (p *KafkaPublisher) publish(ctx context.Context, eventType, tenantID, legalEntityID string, payload any) {
	env := EventEnvelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
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
	p.publish(ctx, "termination.initiated", req.TenantID, req.LegalEntityID, req)
}

func (p *KafkaPublisher) PublishTerminationApproved(ctx context.Context, principalID string, req domain.TerminationRequest) {
	p.publish(ctx, "termination.approved", req.TenantID, req.LegalEntityID, req)
}

func (p *KafkaPublisher) PublishEmployeeTerminated(ctx context.Context, principalID string, req domain.TerminationRequest) {
	p.publish(ctx, "employee.terminated", req.TenantID, req.LegalEntityID, req)
}

func (p *KafkaPublisher) PublishOffboardingCompleted(ctx context.Context, principalID string, chk domain.OffboardingChecklist) {
	p.publish(ctx, "offboarding.completed", chk.TenantID, chk.LegalEntityID, chk)
}
