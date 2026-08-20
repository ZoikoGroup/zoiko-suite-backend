package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/employment-contracts-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.EmploymentContract carries
// real TenantID/LegalEntityID; domain.ContractAmendment carries a real
// AmendedBy actor. Neither struct has a jurisdiction field, and
// EmploymentContract has no actor field of its own, so actor_id is
// threaded through explicitly for issue/terminate from each handler's
// already-verified principalID.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the
	// interface itself non-nil, defeating the p.producer == nil check in
	// emit(). Keep the field genuinely nil in that case.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishContractIssued(ctx context.Context, correlationID, actorID string, c domain.EmploymentContract) {
	p.emit(ctx, "employment.contract.issued", correlationID, c.TenantID, c.LegalEntityID, actorID, c.ContractID, map[string]any{
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
	p.emit(ctx, "employment.contract.amended", correlationID, c.TenantID, c.LegalEntityID, amd.AmendedBy, c.ContractID, map[string]any{
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

func (p *Publisher) PublishContractTerminated(ctx context.Context, correlationID, actorID string, c domain.EmploymentContract) {
	p.emit(ctx, "employment.contract.terminated", correlationID, c.TenantID, c.LegalEntityID, actorID, c.ContractID, map[string]any{
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

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "employment-contracts-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
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
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
