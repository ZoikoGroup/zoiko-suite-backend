// Package outbox drains the WFP transactional outbox to Kafka.
//
// The store decides that an event HAPPENED, in the same transaction as the state
// change; this package decides when it is DELIVERED. Delivery may fail and be
// retried; the fact may not be lost. A broker outage therefore delays WFP events
// and never discards them.
package outbox

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/store"
)

// Claimer is the store surface the relay needs.
type Claimer interface {
	ClaimOutbox(ctx context.Context, limit int, fn func([]store.OutboxRecord) error) error
}

// Writer is the Kafka surface the relay needs (*kafka.Writer satisfies it).
type Writer interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

const (
	DefaultBatchSize = 200
	DefaultInterval  = 250 * time.Millisecond
)

type Relay struct {
	store     Claimer
	writer    Writer
	log       *zap.Logger
	batchSize int
	interval  time.Duration
}

func NewRelay(s Claimer, w Writer, log *zap.Logger) *Relay {
	return &Relay{store: s, writer: w, log: log, batchSize: DefaultBatchSize, interval: DefaultInterval}
}

// Run drains until ctx is cancelled. A drain that filled its batch loops again
// with no wait: a backlog is exactly when sleeping between batches is wrong.
func (r *Relay) Run(ctx context.Context) {
	r.log.Info("wfp outbox relay started", zap.Int("batch_size", r.batchSize), zap.Duration("idle_interval", r.interval))
	for {
		n, err := r.DrainOnce(ctx)
		if ctx.Err() != nil {
			r.log.Info("wfp outbox relay stopped")
			return
		}
		if err != nil {
			r.log.Error("wfp outbox drain failed", zap.Error(err))
		}
		if n == r.batchSize && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			r.log.Info("wfp outbox relay stopped")
			return
		case <-time.After(r.interval):
		}
	}
}

// DrainOnce claims one batch and publishes it in a single Kafka call, returning
// how many events were published.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	published := 0
	err := r.store.ClaimOutbox(ctx, r.batchSize, func(recs []store.OutboxRecord) error {
		msgs := make([]kafka.Message, 0, len(recs))
		for _, rec := range recs {
			msgs = append(msgs, kafka.Message{Key: []byte(rec.Key), Value: rec.Body})
		}
		if err := r.writer.WriteMessages(ctx, msgs...); err != nil {
			return err
		}
		published = len(recs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return published, nil
}
