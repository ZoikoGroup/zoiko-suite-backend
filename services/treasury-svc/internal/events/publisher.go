package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
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

// PublishCashPositionUpdated emits the cash.position.updated event.
func (p *Publisher) PublishCashPositionUpdated(ctx context.Context, correlationID string, balance domain.CashBalance) {
	p.emit(ctx, "cash.position.updated", correlationID, map[string]any{
		"tenant_id":         balance.TenantID,
		"bank_account_id":   balance.BankAccountID,
		"ledger_balance":    balance.LedgerBalance,
		"available_balance": balance.AvailableBalance,
		"as_of_timestamp":   balance.AsOfTimestamp,
	})
}

// PublishEffectiveCashUpdated emits the effective.cash.position.updated event.
func (p *Publisher) PublishEffectiveCashUpdated(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse) {
	p.emit(ctx, "effective.cash.position.updated", correlationID, map[string]any{
		"tenant_id":                resp.TenantID,
		"legal_entity_id":           resp.LegalEntityID,
		"currency_code":            resp.CurrencyCode,
		"effective_available_cash": resp.EffectiveAvailableCash,
		"as_of_timestamp":          resp.AsOfTimestamp,
	})
}

// PublishLiquidityThresholdBreached emits the liquidity.threshold.breached event.
func (p *Publisher) PublishLiquidityThresholdBreached(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse) {
	escalationEmail := ""
	minRequired := 0.0
	if resp.ThresholdDetails != nil {
		minRequired = resp.ThresholdDetails.MinimumRequiredBalance
	}
	p.emit(ctx, "liquidity.threshold.breached", correlationID, map[string]any{
		"tenant_id":                resp.TenantID,
		"legal_entity_id":           resp.LegalEntityID,
		"currency_code":            resp.CurrencyCode,
		"minimum_required_balance": minRequired,
		"effective_available_cash": resp.EffectiveAvailableCash,
		"escalation_email":        escalationEmail,
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
		SourceService: "treasury-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
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
