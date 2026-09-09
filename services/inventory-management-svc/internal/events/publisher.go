package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every
// published event carries event name, event version, timestamp, tenant
// ID, legal entity ID, actor ID, correlation ID, source service, and
// payload schema version.
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

// MessageWriter is the one method Publisher needs from *kafka.Writer —
// narrowed to an interface so tests can assert envelope content without a
// live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

func (p *Publisher) PublishInventoryItemCreated(ctx context.Context, correlationID, actorID string, it domain.InventoryItem) {
	p.emit(ctx, "inventory.item.created", correlationID, it.TenantID, it.LegalEntityID, actorID, it.ItemID, map[string]any{
		"item_id": it.ItemID, "legal_entity_id": it.LegalEntityID, "sku": it.SKU, "status": it.Status,
	})
}

func (p *Publisher) PublishInventoryItemActivated(ctx context.Context, correlationID, actorID string, it domain.InventoryItem) {
	p.emit(ctx, "inventory.item.activated", correlationID, it.TenantID, it.LegalEntityID, actorID, it.ItemID, map[string]any{
		"item_id": it.ItemID, "activated_at": it.ActivatedAt,
	})
}

func (p *Publisher) PublishInventoryPolicyChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, itemID, policyType string) {
	p.emit(ctx, "inventory.policy.changed", correlationID, tenantID, legalEntityID, actorID, itemID, map[string]any{
		"item_id": itemID, "policy_type": policyType,
	})
}

func (p *Publisher) PublishInventoryItemRetired(ctx context.Context, correlationID, actorID string, it domain.InventoryItem) {
	p.emit(ctx, "inventory.item.retired", correlationID, it.TenantID, it.LegalEntityID, actorID, it.ItemID, map[string]any{
		"item_id": it.ItemID, "retirement_reason": it.RetirementReason, "retired_at": it.RetiredAt,
	})
}

// INV-02's own named events: "InventoryLocationCreated;
// InventoryLocationActivated; InventoryLocationQuarantined;
// InventoryLocationChanged; InventoryLocationRetired."

func (p *Publisher) PublishInventoryLocationCreated(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation) {
	p.emit(ctx, "inventory.location.created", correlationID, l.TenantID, l.LegalEntityID, actorID, l.LocationID, map[string]any{
		"location_id": l.LocationID, "legal_entity_id": l.LegalEntityID, "location_code": l.LocationCode, "status": l.Status,
	})
}

func (p *Publisher) PublishInventoryLocationActivated(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation) {
	p.emit(ctx, "inventory.location.activated", correlationID, l.TenantID, l.LegalEntityID, actorID, l.LocationID, map[string]any{
		"location_id": l.LocationID, "activated_at": l.ActivatedAt,
	})
}

func (p *Publisher) PublishInventoryLocationQuarantined(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation) {
	p.emit(ctx, "inventory.location.quarantined", correlationID, l.TenantID, l.LegalEntityID, actorID, l.LocationID, map[string]any{
		"location_id": l.LocationID, "quarantine_reason": l.QuarantineReason, "quarantined_at": l.QuarantinedAt,
	})
}

func (p *Publisher) PublishInventoryLocationChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, locationID, changeType string) {
	p.emit(ctx, "inventory.location.changed", correlationID, tenantID, legalEntityID, actorID, locationID, map[string]any{
		"location_id": locationID, "change_type": changeType,
	})
}

func (p *Publisher) PublishInventoryLocationRetired(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation) {
	p.emit(ctx, "inventory.location.retired", correlationID, l.TenantID, l.LegalEntityID, actorID, l.LocationID, map[string]any{
		"location_id": l.LocationID, "retirement_reason": l.RetirementReason, "retired_at": l.RetiredAt,
	})
}

// INV-03's own named events (a subset — "InventoryMovementExceptionRaised"
// is not wired in, since this v1 has no exception-detection logic yet;
// stated honestly in the findings doc): "InventoryMovementCommitted;
// InventoryMovementReversed; InventoryTransferred; InventoryReceived;
// InventoryIssued."

func (p *Publisher) PublishInventoryMovementCommitted(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement) {
	p.emit(ctx, "inventory.movement.committed", correlationID, m.TenantID, m.LegalEntityID, actorID, m.MovementID, map[string]any{
		"movement_id": m.MovementID, "item_id": m.ItemID, "movement_type": m.MovementType, "quantity": m.Quantity,
	})
}

func (p *Publisher) PublishInventoryMovementReversed(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement) {
	p.emit(ctx, "inventory.movement.reversed", correlationID, m.TenantID, m.LegalEntityID, actorID, m.MovementID, map[string]any{
		"movement_id": m.MovementID, "reverses_movement_id": m.ReversesMovementID, "supersedes_movement_id": m.SupersedesMovementID,
	})
}

func (p *Publisher) PublishInventoryTransferred(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement) {
	p.emit(ctx, "inventory.transferred", correlationID, m.TenantID, m.LegalEntityID, actorID, m.MovementID, map[string]any{
		"movement_id": m.MovementID, "item_id": m.ItemID, "quantity": m.Quantity,
	})
}

func (p *Publisher) PublishInventoryReceived(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement) {
	p.emit(ctx, "inventory.received", correlationID, m.TenantID, m.LegalEntityID, actorID, m.MovementID, map[string]any{
		"movement_id": m.MovementID, "item_id": m.ItemID, "quantity": m.Quantity,
	})
}

func (p *Publisher) PublishInventoryIssued(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement) {
	p.emit(ctx, "inventory.issued", correlationID, m.TenantID, m.LegalEntityID, actorID, m.MovementID, map[string]any{
		"movement_id": m.MovementID, "item_id": m.ItemID, "quantity": m.Quantity,
	})
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "inventory-management-svc",
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
			zap.String("event_type", eventType), zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)))
	}
}
