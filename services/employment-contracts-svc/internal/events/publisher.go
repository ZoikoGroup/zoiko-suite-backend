package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/employment-contracts-svc/internal/domain"
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

func (p *Publisher) PublishContractIssued(ctx context.Context, correlationID string, c domain.EmploymentContract) {
	p.emit(ctx, "employment.contract.issued", correlationID, map[string]any{
		"contract_id":        c.ContractID,
		"tenant_id":          c.TenantID,
		"legal_entity_id":    c.LegalEntityID,
		"employee_id":        c.EmployeeID,
		"contract_number":    c.ContractNumber,
		"version":            c.Version,
		"contract_type":      c.ContractType,
		"status":             c.Status,
		"base_salary_amount": c.BaseSalaryAmount,
		"currency":           c.Currency,
		"pay_frequency":      c.PayFrequency,
		"effective_from":     c.EffectiveFrom,
		"issued_at":          c.CreatedAt,
	})
}

func (p *Publisher) PublishContractAmended(ctx context.Context, correlationID string, c domain.EmploymentContract, amd domain.ContractAmendment) {
	p.emit(ctx, "employment.contract.amended", correlationID, map[string]any{
		"contract_id":        c.ContractID,
		"tenant_id":          c.TenantID,
		"legal_entity_id":    c.LegalEntityID,
		"employee_id":        c.EmployeeID,
		"contract_number":    c.ContractNumber,
		"from_version":       amd.FromVersion,
		"to_version":         amd.ToVersion,
		"amendment_reason":   amd.AmendmentReason,
		"amended_by":         amd.AmendedBy,
		"base_salary_amount": c.BaseSalaryAmount,
		"currency":           c.Currency,
		"effective_from":     amd.EffectiveFrom,
		"amended_at":         amd.CreatedAt,
	})
}

func (p *Publisher) PublishContractTerminated(ctx context.Context, correlationID string, c domain.EmploymentContract) {
	p.emit(ctx, "employment.contract.terminated", correlationID, map[string]any{
		"contract_id":     c.ContractID,
		"tenant_id":       c.TenantID,
		"legal_entity_id": c.LegalEntityID,
		"employee_id":     c.EmployeeID,
		"contract_number": c.ContractNumber,
		"version":         c.Version,
		"effective_to":    c.EffectiveTo,
		"terminated_at":   time.Now().UTC(),
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
		SourceService: "employment-contracts-svc",
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