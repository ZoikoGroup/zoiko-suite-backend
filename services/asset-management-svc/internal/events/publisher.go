package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
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

func (p *Publisher) PublishAssetRegistered(ctx context.Context, correlationID, actorID string, a domain.FixedAsset) {
	p.emit(ctx, "asset.registered", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "legal_entity_id": a.LegalEntityID, "status": a.Status, "registered_at": a.RegisteredAt,
	})
}

func (p *Publisher) PublishAssetComponentAdded(ctx context.Context, correlationID, actorID string, a domain.FixedAsset, c domain.AssetComponent) {
	p.emit(ctx, "asset.component.added", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "component_id": c.ComponentID, "description": c.Description,
	})
}

func (p *Publisher) PublishAssetBookAssigned(ctx context.Context, correlationID, actorID string, a domain.FixedAsset, b domain.AssetBookAssignment) {
	p.emit(ctx, "asset.book.assigned", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "assignment_id": b.AssignmentID, "book_id": b.BookID,
	})
}

func (p *Publisher) PublishAssetCapitalizationRequested(ctx context.Context, correlationID, actorID string, a domain.FixedAsset) {
	p.emit(ctx, "asset.capitalization.requested", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "capitalized_at": a.CapitalizedAt,
	})
}

func (p *Publisher) PublishAssetMetadataChanged(ctx context.Context, correlationID, actorID string, a domain.FixedAsset) {
	p.emit(ctx, "asset.metadata.changed", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "custodian_id": a.CustodianID, "location_id": a.LocationID, "description": a.Description,
	})
}

func (p *Publisher) PublishAssetSuspended(ctx context.Context, correlationID, actorID string, a domain.FixedAsset) {
	p.emit(ctx, "asset.suspended", correlationID, a.TenantID, a.LegalEntityID, actorID, a.AssetID, map[string]any{
		"asset_id": a.AssetID, "suspension_reason": a.SuspensionReason, "suspended_at": a.SuspendedAt,
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
		SourceService: "asset-management-svc",
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
