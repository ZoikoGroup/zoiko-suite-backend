// Package outbox relays the transactional outbox to Kafka. Store writes the
// obligation mutation and its event in one transaction; this worker delivers
// the stored envelope, at least once, for as long as the broker is down.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/store"
)

const (
	pollInterval  = 500 * time.Millisecond
	statsInterval = 15 * time.Second
	batchSize     = 100
)

// Source is the outbox persistence the worker drives. RelayOutbox claims due
// rows (FOR UPDATE SKIP LOCKED), calls publish for each, and records the
// outcome — published, or retried later with backoff — in the same
// transaction.
type Source interface {
	RelayOutbox(ctx context.Context, limit int, publish func(ctx context.Context, e store.OutboxEvent) error) (published, failed int, err error)
	GetOutboxStats(ctx context.Context) (store.OutboxStats, error)
}

// Publisher writes one already-built envelope to the event backbone.
type Publisher interface {
	PublishEnvelope(ctx context.Context, key string, envelope []byte) error
}

// Gauges receives the outbox state for alerting (pending growth, parked
// events, age of the oldest undelivered event). nil disables it.
type Gauges interface {
	SetOutbox(pending, failed int, oldestPendingSeconds float64)
}

// Worker polls the outbox and publishes pending events.
type Worker struct {
	source    Source
	publisher Publisher
	gauges    Gauges
	log       *zap.Logger

	cancel context.CancelFunc
	doneCh chan struct{}
}

// NewWorker constructs an outbox worker.
func NewWorker(source Source, publisher Publisher, log *zap.Logger) *Worker {
	return &Worker{
		source:    source,
		publisher: publisher,
		log:       log,
		doneCh:    make(chan struct{}),
	}
}

// WithGauges reports the outbox state to g.
func (w *Worker) WithGauges(g Gauges) *Worker {
	w.gauges = g
	return w
}

// Start begins the polling loop in a background goroutine.
func (w *Worker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.run(ctx)
}

// Stop cancels the loop and waits for it to finish. Rows not marked published
// stay pending and are relayed again.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	<-w.doneCh
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.doneCh)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	stats := time.NewTicker(statsInterval)
	defer stats.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox worker stopped")
			return
		case <-ticker.C:
			if _, err := w.RelayOnce(ctx); err != nil && ctx.Err() == nil {
				w.log.Error("outbox: relay pass failed", zap.Error(err))
			}
		case <-stats.C:
			w.ReportStats(ctx)
		}
	}
}

// RelayOnce publishes one batch of due events. A failed publish is retried
// later with backoff; it is never dropped.
func (w *Worker) RelayOnce(ctx context.Context) (int, error) {
	published, failed, err := w.source.RelayOutbox(ctx, batchSize, func(ctx context.Context, e store.OutboxEvent) error {
		return w.publisher.PublishEnvelope(ctx, MessageKey(e), e.EventPayload)
	})
	if failed > 0 {
		w.log.Warn("outbox: publishes failed, will retry with backoff", zap.Int("failed", failed), zap.Int("published", published))
	}
	return published, err
}

// ReportStats reads the outbox state into the gauges.
func (w *Worker) ReportStats(ctx context.Context) {
	if w.gauges == nil {
		return
	}
	st, err := w.source.GetOutboxStats(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("outbox: stats read failed", zap.Error(err))
		}
		return
	}
	w.gauges.SetOutbox(st.Pending, st.Failed, st.OldestPendingSecond)
}

// MessageKey is the Kafka partition key: tenant and aggregate (Event Catalogue
// §5.3 "hash(tenant, aggregate)"), so every event about one obligation keeps
// its order. The outbox id is the fallback for an event that is not about one
// obligation (definitions, mappings).
func MessageKey(event store.OutboxEvent) string {
	var env struct {
		Payload struct {
			ObligationID string `json:"obligation_id"`
			DefinitionID string `json:"obligation_definition_id"`
			MappingID    string `json:"mapping_id"`
		} `json:"payload"`
	}
	aggregate := event.OutboxID
	if err := json.Unmarshal(event.EventPayload, &env); err == nil {
		switch {
		case env.Payload.ObligationID != "":
			aggregate = env.Payload.ObligationID
		case env.Payload.MappingID != "":
			aggregate = env.Payload.MappingID
		case env.Payload.DefinitionID != "":
			aggregate = env.Payload.DefinitionID
		}
	}
	return event.TenantID + "/" + aggregate
}
