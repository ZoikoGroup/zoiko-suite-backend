package decisionledger

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Gauges is what a writer's /metrics exposes about its relay. Each writer
// implements it with its own metric names (prometheus is the writers'
// dependency, not this module's).
type Gauges interface {
	SetBacklog(b Backlog)
	Delivered()
	Failed(parked bool)
	Purged(n int64)
}

// Observe reports the relay's deliveries to g.
func (r *Relay) Observe(g Gauges) {
	r.OnDelivered = g.Delivered
	r.OnFailed = g.Failed
}

// RunSampler refreshes g's backlog every interval (alert on the oldest pending
// age and on any parked record) and, when retention is positive, purges
// delivered rows older than retention once an hour. It returns when ctx ends.
func RunSampler(ctx context.Context, q *PgQueue, g Gauges, interval, retention time.Duration, log *zap.Logger) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastPurge time.Time
	for {
		if b, err := q.Backlog(ctx); err != nil {
			if ctx.Err() == nil {
				log.Warn("decision-log outbox: backlog read failed", zap.Error(err))
			}
		} else {
			g.SetBacklog(b)
		}
		if retention > 0 && time.Since(lastPurge) >= time.Hour {
			lastPurge = time.Now()
			if n, err := q.PurgeDelivered(ctx, retention); err != nil {
				if ctx.Err() == nil {
					log.Warn("decision-log outbox: purge failed", zap.Error(err))
				}
			} else {
				g.Purged(n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
