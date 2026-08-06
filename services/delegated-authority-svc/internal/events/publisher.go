package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/delegated-authority-svc/internal/domain"
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

func (p *Publisher) PublishDelegated(ctx context.Context, d domain.DelegationGrant) {
	p.emit(ctx, "authority.delegated", d.CorrelationID, map[string]any{
		"delegation_id":          d.DelegationID,
		"legal_entity_id":        d.LegalEntityID,
		"delegator_principal_id": d.DelegatorPrincipalID,
		"delegate_principal_id":  d.DelegatePrincipalID,
		"action_type":            d.ActionType,
		"effective_from":         d.EffectiveFrom,
		"effective_to":           d.EffectiveTo,
	})
}

func (p *Publisher) PublishRevoked(ctx context.Context, d domain.DelegationGrant) {
	p.emit(ctx, "authority.revoked", d.CorrelationID, map[string]any{
		"delegation_id":   d.DelegationID,
		"legal_entity_id": d.LegalEntityID,
	})
}

func (p *Publisher) PublishExpired(ctx context.Context, d domain.DelegationGrant) {
	p.emit(ctx, "authority.expired", d.CorrelationID, map[string]any{
		"delegation_id":   d.DelegationID,
		"legal_entity_id": d.LegalEntityID,
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
		SourceService: "delegated-authority-svc",
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
