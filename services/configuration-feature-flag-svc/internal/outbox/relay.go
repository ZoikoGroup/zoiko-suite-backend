// Package outbox drains the transactional outbox to Kafka.
//
// The split is the whole design: the store decides that an event HAPPENED, in
// the same transaction as the state change, and this package decides when it is
// DELIVERED. Delivery is allowed to fail and be retried; the fact is not
// allowed to be lost.
//
// Before this existed the two were the same act — a Kafka write from the
// handler after the commit, with the error logged and discarded — so a broker
// hiccup during a config write threw away the only notice that the value had
// changed and answered 201. The consumer went on serving the superseded value,
// which is valid data and therefore undetectable downstream.
package outbox

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/store"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
)

// Claimer is the store surface the relay needs.
type Claimer interface {
	ClaimOutbox(ctx context.Context, limit int, fn func([]store.OutboxRecord) error) error
	OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error)
}

// Sender is the publisher surface the relay needs.
type Sender interface {
	Publish(ctx context.Context, msgs []kafka.Message) error
}

const (
	// DefaultBatchSize is how many events one drain claims. Large enough that a
	// backlog clears in few round trips, small enough that a failing batch does
	// not hold a long write lock on the outbox.
	DefaultBatchSize = 200

	// DefaultInterval is the idle poll. A drain that finds a full batch loops
	// again immediately rather than waiting, so this bounds LATENCY on an idle
	// service, not throughput on a busy one.
	//
	// 250ms rather than a second because of what the events are: a feature flag
	// change is a rollout control, and this number plus the consumer lag is the
	// gap between an operator switching a feature off and it actually being off
	// for the people using it.
	DefaultInterval = 250 * time.Millisecond
)

// Relay drains the outbox in a background loop.
type Relay struct {
	store     Claimer
	publisher Sender
	metrics   *telemetry.Domain
	log       *zap.Logger
	batchSize int
	interval  time.Duration
}

// NewRelay constructs a Relay with the default batch size and poll interval.
func NewRelay(s Claimer, p Sender, m *telemetry.Domain, log *zap.Logger) *Relay {
	return &Relay{
		store:     s,
		publisher: p,
		metrics:   m,
		log:       log,
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
	}
}

// Run drains until ctx is cancelled.
//
// A drain that filled its batch loops again with no wait: a backlog is exactly
// the situation where sleeping between batches is wrong, and the fixed-tick
// version of this loop drains at batchSize/interval events per second no matter
// how far behind it is.
func (r *Relay) Run(ctx context.Context) {
	r.log.Info("outbox relay started",
		zap.Int("batch_size", r.batchSize),
		zap.Duration("idle_interval", r.interval),
	)
	for {
		n, err := r.DrainOnce(ctx)
		if ctx.Err() != nil {
			r.log.Info("outbox relay stopped")
			return
		}
		if err != nil {
			r.log.Error("outbox drain failed", zap.Error(err))
		}
		r.observeDepth(ctx)
		if n == r.batchSize && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopped")
			return
		case <-time.After(r.interval):
		}
	}
}

// DrainOnce claims one batch and publishes it. Returns how many events were
// published.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	published := 0
	err := r.store.ClaimOutbox(ctx, r.batchSize, func(recs []store.OutboxRecord) error {
		msgs := make([]kafka.Message, 0, len(recs))
		for _, rec := range recs {
			msgs = append(msgs, kafka.Message{Key: []byte(rec.Key), Value: rec.Body})
		}
		// One call, not a loop — see Publisher.Publish. A per-record loop waits
		// out kafka-go's BatchTimeout once per event.
		if err := r.publisher.Publish(ctx, msgs); err != nil {
			r.metrics.OutboxFailures.Inc()
			return err
		}
		for _, rec := range recs {
			r.metrics.OutboxPublished.WithLabelValues(rec.EventType).Inc()
		}
		published = len(recs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return published, nil
}

// observeDepth refreshes the backlog gauges.
//
// Kept out of DrainOnce so the numbers are reported even on a tick where the
// drain itself failed — which is precisely the tick where a growing backlog is
// the thing worth seeing.
func (r *Relay) observeDepth(ctx context.Context) {
	pending, oldest, err := r.store.OutboxDepth(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("outbox depth query failed", zap.Error(err))
		}
		return
	}
	r.metrics.OutboxPending.Set(float64(pending))
	r.metrics.OutboxOldestAgeSeconds.Set(oldest.Seconds())
}
