// Package outbox delivers events from risk_outbox_events to Kafka.
package outbox

import (
	"context"
	"time"

	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

// EnvelopeWriter is the subset of events.Publisher the relay needs.
type EnvelopeWriter interface {
	WriteEnvelope(ctx context.Context, tenantID string, envelope []byte) error
}

// Relay polls the outbox and publishes due events, at least once. Consumers
// dedupe on event_id.
type Relay struct {
	store        *store.Store
	writer       EnvelopeWriter
	logger       *zap.Logger
	pollInterval time.Duration
	batchSize    int
}

// NewRelay builds a relay polling every pollInterval.
func NewRelay(s *store.Store, writer EnvelopeWriter, logger *zap.Logger, pollInterval time.Duration) *Relay {
	return &Relay{store: s, writer: writer, logger: logger, pollInterval: pollInterval, batchSize: 50}
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
			if _, err := r.RelayOnce(ctx); err != nil && ctx.Err() == nil {
				r.logger.Warn("outbox relay pass failed", zap.Error(err))
			}
		}
	}
}

// RelayOnce publishes one batch and returns how many events were delivered.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	batch, err := r.store.ClaimOutboxBatch(ctx, r.batchSize)
	if err != nil {
		return 0, err
	}

	var published []string
	failed := map[string]string{}

	for _, event := range batch.Events {
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		writeErr := r.writer.WriteEnvelope(writeCtx, event.TenantID, event.Envelope)
		cancel()

		if writeErr != nil {
			failed[event.ID] = truncate(writeErr.Error(), 500)
			r.logger.Warn("outbox event publish failed; will retry",
				zap.String("event_id", event.ID), zap.String("event_type", event.EventType),
				zap.Int("attempts", event.Attempts+1), zap.Error(writeErr))
			// Stop at the first failure: the broker is probably unavailable,
			// and continuing would hold this batch's row locks for up to
			// 10s per remaining event. Untouched events stay due.
			break
		}
		published = append(published, event.ID)
	}

	if err := batch.Finish(ctx, published, failed); err != nil {
		return 0, err
	}
	return len(published), nil
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max]
}
