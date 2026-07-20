package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/financial-close-svc/internal/domain"
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

func (p *Publisher) PublishCloseStarted(ctx context.Context, correlationID string, fp domain.FiscalPeriod) {
	p.emit(ctx, "period.close.started", correlationID, map[string]any{
		"tenant_id":        fp.TenantID,
		"legal_entity_id":  fp.LegalEntityID,
		"fiscal_period_id": fp.FiscalPeriodID,
		"period_name":      fp.PeriodName,
		"timestamp":        time.Now().UTC(),
	})
}

func (p *Publisher) PublishCloseBlocked(ctx context.Context, correlationID string, fp domain.FiscalPeriod, reasons []string) {
	p.emit(ctx, "period.close.blocked", correlationID, map[string]any{
		"tenant_id":        fp.TenantID,
		"legal_entity_id":  fp.LegalEntityID,
		"fiscal_period_id": fp.FiscalPeriodID,
		"period_name":      fp.PeriodName,
		"reasons":          reasons,
		"timestamp":        time.Now().UTC(),
	})
}

func (p *Publisher) PublishClosed(ctx context.Context, correlationID string, fp domain.FiscalPeriod, evidenceID string) {
	p.emit(ctx, "period.closed", correlationID, map[string]any{
		"tenant_id":            fp.TenantID,
		"legal_entity_id":      fp.LegalEntityID,
		"fiscal_period_id":     fp.FiscalPeriodID,
		"period_name":          fp.PeriodName,
		"evidence_document_id": evidenceID,
		"timestamp":            time.Now().UTC(),
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
		SourceService: "financial-close-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	// Defensively allow publishing to proceed during testing when no producer is connected
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
