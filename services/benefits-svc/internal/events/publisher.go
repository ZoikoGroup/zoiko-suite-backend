package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/benefits-svc/internal/domain"
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

func (p *Publisher) PublishBenefitEnrolled(ctx context.Context, correlationID string, e domain.BenefitElection) {
	p.emit(ctx, "benefit.enrolled", correlationID, map[string]any{
		"election_id":                 e.ElectionID,
		"tenant_id":                   e.TenantID,
		"employee_id":                 e.EmployeeID,
		"plan_id":                     e.PlanID,
		"coverage_level":              e.CoverageLevel,
		"employee_contribution_amount": e.EmployeeContributionAmount,
		"employer_contribution_amount": e.EmployerContributionAmount,
		"effective_from":              e.EffectiveFrom,
		"enrolled_at":                 e.CreatedAt,
	})
}

func (p *Publisher) PublishBenefitChanged(ctx context.Context, correlationID string, e domain.BenefitElection) {
	p.emit(ctx, "benefit.changed", correlationID, map[string]any{
		"election_id":                 e.ElectionID,
		"tenant_id":                   e.TenantID,
		"employee_id":                 e.EmployeeID,
		"plan_id":                     e.PlanID,
		"coverage_level":              e.CoverageLevel,
		"employee_contribution_amount": e.EmployeeContributionAmount,
		"employer_contribution_amount": e.EmployerContributionAmount,
		"updated_at":                  e.UpdatedAt,
	})
}

func (p *Publisher) PublishBenefitTerminated(ctx context.Context, correlationID string, e domain.BenefitElection) {
	p.emit(ctx, "benefit.terminated", correlationID, map[string]any{
		"election_id":    e.ElectionID,
		"tenant_id":      e.TenantID,
		"employee_id":    e.EmployeeID,
		"plan_id":        e.PlanID,
		"effective_to":   e.EffectiveTo,
		"terminated_at":  time.Now().UTC(),
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
		SourceService: "benefits-svc",
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