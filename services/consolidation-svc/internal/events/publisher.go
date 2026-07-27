package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/consolidation-svc/internal/domain"
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

func (p *Publisher) PublishRunStarted(ctx context.Context, correlationID string, run domain.ConsolidationRun) {
	p.emit(ctx, "consolidation.run.started", correlationID, map[string]any{
		"consolidation_run_id":  run.ConsolidationRunID,
		"tenant_id":             run.TenantID,
		"group_legal_entity_id": run.GroupLegalEntityID,
		"fiscal_period":         run.FiscalPeriod,
		"target_currency":       run.TargetCurrency,
		"started_at":            run.StartedAt,
	})
}

func (p *Publisher) PublishCompleted(ctx context.Context, correlationID string, run domain.ConsolidationRun, snapshotCount int) {
	p.emit(ctx, "consolidation.completed", correlationID, map[string]any{
		"consolidation_run_id":  run.ConsolidationRunID,
		"tenant_id":             run.TenantID,
		"group_legal_entity_id": run.GroupLegalEntityID,
		"fiscal_period":         run.FiscalPeriod,
		"status":                run.Status,
		"exception_count":       run.ExceptionCount,
		"snapshot_count":        snapshotCount,
		"completed_at":          time.Now().UTC(),
	})
}

func (p *Publisher) PublishExceptionDetected(ctx context.Context, correlationID string, run domain.ConsolidationRun, exceptions []string) {
	p.emit(ctx, "consolidation.exception.detected", correlationID, map[string]any{
		"consolidation_run_id":  run.ConsolidationRunID,
		"tenant_id":             run.TenantID,
		"group_legal_entity_id": run.GroupLegalEntityID,
		"fiscal_period":         run.FiscalPeriod,
		"exceptions":            exceptions,
		"timestamp":             time.Now().UTC(),
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
		SourceService: "consolidation-svc",
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