package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/vendor-due-diligence-svc/internal/domain"
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

func (p *Publisher) PublishStarted(ctx context.Context, correlationID string, check domain.VendorDDCheck) {
	p.emit(ctx, "vendor.dd.started", correlationID, check.CheckID, map[string]any{
		"check_id":                  check.CheckID,
		"tenant_id":                 check.TenantID,
		"legal_entity_id":           check.LegalEntityID,
		"counterparty_id":           check.CounterpartyID,
		"vendor_name":               check.VendorName,
		"initiated_by_principal_id": check.InitiatedByPrincipalID,
		"started_at":                check.StartedAt,
	})
}

func (p *Publisher) PublishCompleted(ctx context.Context, correlationID string, check domain.VendorDDCheck) {
	p.emit(ctx, "vendor.dd.completed", correlationID, check.CheckID, map[string]any{
		"check_id":        check.CheckID,
		"tenant_id":       check.TenantID,
		"legal_entity_id": check.LegalEntityID,
		"counterparty_id": check.CounterpartyID,
		"vendor_name":     check.VendorName,
		"risk_outcome":    check.RiskOutcome,
		"screening_basis": check.ScreeningBasis,
		// On the event as well as on the API response, and for the same reason. A
		// consumer of this event sees risk_outcome CLEAR and would otherwise have no
		// way to know it came from a hardcoded two-name denylist rather than a
		// sanctions feed. Leaving it off the wire here while putting it on the read
		// API would fix the defect for whoever looks at the console and leave it in
		// place for every automated consumer — which is the more dangerous half.
		// screening_basis is prose and not a contract; this is.
		"screening_source": check.ScreeningSource,
		"completed_at":     check.CompletedAt,
	})
}

func (p *Publisher) PublishFailed(ctx context.Context, correlationID string, check domain.VendorDDCheck, reason string) {
	p.emit(ctx, "vendor.dd.failed", correlationID, check.CheckID, map[string]any{
		"check_id":        check.CheckID,
		"tenant_id":       check.TenantID,
		"legal_entity_id": check.LegalEntityID,
		"counterparty_id": check.CounterpartyID,
		"vendor_name":     check.VendorName,
		"failure_reason":  reason,
		"failed_at":       time.Now().UTC(),
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "vendor-due-diligence-svc",
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
