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

// PublishDepreciationRunAccountingEventEmitted and
// PublishAssetEventAccountingEventEmitted are this service's own
// "accounting event emitted" signals — previously silent (this service
// never published them at all), leaving financial-close-svc's ACC-18
// lineage engine with no way to observe an asset-originated journal.
// Same event_type naming and {run_id/event_id, journal_id} payload shape
// as inventory-management-svc's own
// PublishInventoryAccountingEventEmitted and project-accounting-svc's
// own PublishProjectRecognitionAccountingEventEmitted, so a single
// lineage consumer can handle all three with one small per-event-type
// mapping rather than three different shapes.
func (p *Publisher) PublishDepreciationRunAccountingEventEmitted(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, runID, journalID string) {
	p.emit(ctx, "depreciation.run.accounting_event.emitted", correlationID, tenantID, legalEntityID, actorID, runID, map[string]any{
		"run_id": runID, "journal_id": journalID,
	})
}

func (p *Publisher) PublishAssetEventAccountingEventEmitted(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID, journalID string) {
	p.emit(ctx, "asset.event.accounting_event.emitted", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID, "journal_id": journalID,
	})
}

// AST-02's own remaining named events (verbatim, "Events produced"):
// "DepreciationScheduleBuilt; DepreciationRunCalculated;
// DepreciationRunApproved; DepreciationAccountingEventEmitted;
// DepreciationRunSuperseded." DepreciationAccountingEventEmitted is
// PublishDepreciationRunAccountingEventEmitted above; these four cover
// the rest of the run/schedule lifecycle.

func (p *Publisher) PublishDepreciationScheduleBuilt(ctx context.Context, correlationID, actorID string, s domain.DepreciationSchedule) {
	p.emit(ctx, "depreciation.schedule.built", correlationID, s.TenantID, s.LegalEntityID, actorID, s.ScheduleID, map[string]any{
		"schedule_id": s.ScheduleID, "asset_id": s.AssetID, "book_id": s.BookID, "version": s.Version,
	})
}

func (p *Publisher) PublishDepreciationRunCalculated(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, runID string, lineCount int) {
	p.emit(ctx, "depreciation.run.calculated", correlationID, tenantID, legalEntityID, actorID, runID, map[string]any{
		"run_id": runID, "line_count": lineCount,
	})
}

func (p *Publisher) PublishDepreciationRunApproved(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, runID string) {
	p.emit(ctx, "depreciation.run.approved", correlationID, tenantID, legalEntityID, actorID, runID, map[string]any{
		"run_id": runID,
	})
}

func (p *Publisher) PublishDepreciationRunSuperseded(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, runID string) {
	p.emit(ctx, "depreciation.run.superseded", correlationID, tenantID, legalEntityID, actorID, runID, map[string]any{
		"run_id": runID,
	})
}

// AST-03's own remaining named events (verbatim, "Events produced"):
// "AssetEventCreated; AssetEventApproved; AssetEventApplied;
// AssetImpaired; AssetRevalued; AssetDisposed; AssetEventReversed." All
// seven were previously unpublished — only the accounting-event-emitted
// signal above ever fired.

func (p *Publisher) PublishAssetEventCreated(ctx context.Context, correlationID, actorID string, e domain.AssetEvent) {
	p.emit(ctx, "asset.event.created", correlationID, e.TenantID, e.LegalEntityID, actorID, e.EventID, map[string]any{
		"event_id": e.EventID, "event_type": e.EventType, "asset_id": e.AssetID,
	})
}

func (p *Publisher) PublishAssetEventApproved(ctx context.Context, correlationID, actorID string, e domain.AssetEvent) {
	p.emit(ctx, "asset.event.approved", correlationID, e.TenantID, e.LegalEntityID, actorID, e.EventID, map[string]any{
		"event_id": e.EventID,
	})
}

func (p *Publisher) PublishAssetEventApplied(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID string) {
	p.emit(ctx, "asset.event.applied", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID,
	})
}

func (p *Publisher) PublishAssetImpaired(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID, assetID string, amount *float64) {
	p.emit(ctx, "asset.impaired", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID, "asset_id": assetID, "amount": amount,
	})
}

func (p *Publisher) PublishAssetRevalued(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID, assetID string, amount *float64) {
	p.emit(ctx, "asset.revalued", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID, "asset_id": assetID, "amount": amount,
	})
}

func (p *Publisher) PublishAssetDisposed(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID, assetID string, proceeds *float64) {
	p.emit(ctx, "asset.disposed", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID, "asset_id": assetID, "proceeds_amount": proceeds,
	})
}

// PublishAssetEventReversed covers both REVERSED and SUPERSEDED outcomes
// — status distinguishes which — since correctAssetEvent is the one
// real store path both correction commands funnel into.
func (p *Publisher) PublishAssetEventReversed(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, eventID, status string) {
	p.emit(ctx, "asset.event.reversed", correlationID, tenantID, legalEntityID, actorID, eventID, map[string]any{
		"event_id": eventID, "status": status,
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
