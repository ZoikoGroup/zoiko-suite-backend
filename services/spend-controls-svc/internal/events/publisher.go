package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/spend-controls-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer *kafka.Writer
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishThresholdBreached(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy, projected float64) {
	p.emit(ctx, "spend.threshold.breached", correlationID, map[string]any{
		"spend_policy_id":  policy.SpendPolicyID,
		"legal_entity_id":  check.LegalEntityID,
		"category":         check.Category,
		"attempted_amount": check.Amount,
		"projected_total":  projected,
		"threshold_amount": policy.ThresholdAmount,
		"currency_code":    check.CurrencyCode,
	})
}

func (p *Publisher) PublishBlockApplied(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy) {
	p.emit(ctx, "spend.block.applied", correlationID, map[string]any{
		"spend_policy_id":  policy.SpendPolicyID,
		"legal_entity_id":  check.LegalEntityID,
		"category":         check.Category,
		"source_reference": check.SourceReference,
		"blocked_at":       time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "spend-controls-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if p.producer == nil {
		p.log.Info("simulating publish event in dry mode", zap.String("event_type", eventType))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
