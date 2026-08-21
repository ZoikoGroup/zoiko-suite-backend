package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-exceptions-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.PayrollException has no
// legal_entity_id field of its own — the handler resolves one from the
// affected employee when there is one, and uses "GLOBAL" as a sentinel
// for the authz check when there isn't. That sentinel is not a real
// legal entity, so callers must pass "" (never "GLOBAL") for
// legalEntityID here — the envelope omits it rather than fabricating
// scope. actor_id is threaded through explicitly (ResolvedBy already
// exists on the struct for the resolved event but is sourced from the
// same principalID for consistency).
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

func (p *Publisher) PublishExceptionRaised(ctx context.Context, correlationID, legalEntityID, actorID string, e domain.PayrollException) {
	p.emit(ctx, "payroll.exception.raised", correlationID, e.TenantID, legalEntityID, actorID, e.ExceptionID, map[string]any{
		"exception_id":   e.ExceptionID,
		"tenant_id":      e.TenantID,
		"payroll_run_id": e.PayrollRunID,
		"employee_id":    e.EmployeeID,
		"exception_code": e.ExceptionCode,
		"severity":       e.Severity,
		"description":    e.Description,
		"raised_at":      e.CreatedAt,
	})
}

func (p *Publisher) PublishExceptionResolved(ctx context.Context, correlationID, legalEntityID string, e domain.PayrollException) {
	actorID := ""
	if e.ResolvedBy != nil {
		actorID = *e.ResolvedBy
	}
	p.emit(ctx, "payroll.exception.resolved", correlationID, e.TenantID, legalEntityID, actorID, e.ExceptionID, map[string]any{
		"exception_id":     e.ExceptionID,
		"tenant_id":        e.TenantID,
		"payroll_run_id":   e.PayrollRunID,
		"status":           e.Status,
		"resolution_notes": e.ResolutionNotes,
		"resolved_by":      e.ResolvedBy,
		"resolved_at":      e.ResolvedAt,
	})
}

func (p *Publisher) PublishBlockerFlagged(ctx context.Context, correlationID, tenantID, legalEntityID, actorID, payrollRunID string, blockerCount int) {
	p.emit(ctx, "payroll.blocker.flagged", correlationID, tenantID, legalEntityID, actorID, payrollRunID, map[string]any{
		"payroll_run_id": payrollRunID,
		"blocker_count":  blockerCount,
		"flagged_at":     time.Now().UTC(),
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
		SourceService: "payroll-exceptions-svc",
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
