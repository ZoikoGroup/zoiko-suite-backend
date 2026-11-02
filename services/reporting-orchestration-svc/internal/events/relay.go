package events

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// OutboxSource is the store method the relay needs (store.PgStore).
type OutboxSource interface {
	PublishOutboxBatch(ctx context.Context, limit int, publish func(context.Context, store.PendingOutboxEvent) error) (int, error)
}

// Relay is the transactional-outbox publisher: it polls outbox_events and
// publishes unpublished rows to Kafka. Handlers never publish directly — the
// event row is committed with the state change, and the relay delivers it
// at least once. Consumers deduplicate on event_id (= outbox_event_id).
type Relay struct {
	source       OutboxSource
	publisher    *Publisher
	logger       *zap.Logger
	pollInterval time.Duration
	batchSize    int
}

func NewRelay(source OutboxSource, publisher *Publisher, logger *zap.Logger) *Relay {
	return &Relay{source: source, publisher: publisher, logger: logger, pollInterval: time.Second, batchSize: 50}
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.publishOnce(ctx)
		}
	}
}

// publishOnce relays one batch and logs the outcome.
func (r *Relay) publishOnce(ctx context.Context) {
	published, err := r.source.PublishOutboxBatch(ctx, r.batchSize, r.publishEvent)
	if err != nil {
		r.logger.Error("outbox relay batch failed", zap.Error(err))
		return
	}
	if published > 0 {
		r.logger.Debug("outbox relay published events", zap.Int("count", published))
	}
}

// publishEvent sends one outbox row to Kafka with a bounded timeout.
func (r *Relay) publishEvent(ctx context.Context, event store.PendingOutboxEvent) error {
	publishCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.publisher.Publish(publishCtx, PublishParams{
		EventID:       event.OutboxEventID,
		OccurredAt:    event.CreatedAt,
		EventType:     event.EventType,
		SubjectID:     event.AggregateID,
		TenantID:      event.TenantID,
		LegalEntityID: event.LegalEntityID,
		ActorID:       event.ActorID,
		CorrelationID: event.CorrelationID,
		Payload:       json.RawMessage(event.PayloadJSON),
	})
}
