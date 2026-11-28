package decisionledger

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	batchSize = 50
	// lease must outlast one batch of ledger calls.
	lease      = 5 * time.Minute
	maxBackoff = 10 * time.Minute
)

// Relay drains a writer's decision_log_outbox into the ledger.
type Relay struct {
	queue    Queue
	sender   Sender
	interval time.Duration
	log      *zap.Logger
	now      func() time.Time

	// OnDelivered and OnFailed, when set, count deliveries for the writer's
	// metrics.
	OnDelivered func()
	OnFailed    func(parked bool)

	// gap is the least time between two deliveries (SetMaxPerSecond); 0 =
	// unpaced.
	gap time.Duration

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewRelay builds a relay; interval <= 0 means two seconds.
func NewRelay(q Queue, s Sender, interval time.Duration, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Relay{queue: q, sender: s, interval: interval, log: log,
		now:  func() time.Time { return time.Now().UTC() },
		stop: make(chan struct{}), done: make(chan struct{})}
}

// SetMaxPerSecond caps deliveries per second (0 or less = no cap). The ledger
// write costs the ledger an authorization check of its own; under a burst the
// cap lets the record wait in the outbox (it is never lost) instead of
// competing with the decisions the writer is answering. Measured 25 Nov 2026
// on authorization-svc: unpaced delivery during a DENY-heavy load raised the
// writer's p95 by 16-29 %, the in-transaction enqueue alone by 7 %.
func (r *Relay) SetMaxPerSecond(n int) {
	if n <= 0 {
		r.gap = 0
		return
	}
	r.gap = time.Second / time.Duration(n)
}

// Start runs the relay until Stop.
func (r *Relay) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.interval)
		defer t.Stop()
		r.log.Info("decision-log relay started", zap.Duration("interval", r.interval))
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				// A full batch means more are waiting: drain without waiting
				// for the next tick.
				for {
					ctx, cancel := context.WithTimeout(context.Background(), lease)
					n := r.RunOnce(ctx)
					cancel()
					if n < batchSize {
						break
					}
					select {
					case <-r.stop:
						return
					default:
					}
				}
			}
		}
	}()
}

// Stop ends the loop and waits for an in-flight batch to finish.
func (r *Relay) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// backoff is 2^attempts seconds, capped at maxBackoff.
func backoff(attempts int) time.Duration {
	if attempts > 10 {
		return maxBackoff
	}
	d := time.Duration(1<<uint(attempts)) * time.Second
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// RunOnce delivers one batch and reports how many rows it claimed.
func (r *Relay) RunOnce(ctx context.Context) int {
	batch, err := r.queue.Claim(ctx, batchSize, lease)
	if err != nil {
		r.log.Error("decision-log relay: claim failed", zap.Error(err))
		return 0
	}
	for i, p := range batch {
		if i > 0 && r.gap > 0 {
			select {
			case <-ctx.Done():
				// The rest stay leased; their lease runs out and they are
				// claimed again.
				return len(batch)
			case <-r.stop:
				return len(batch)
			case <-time.After(r.gap):
			}
		}
		err := r.sender.Send(ctx, p.Record)
		switch {
		case err == nil:
			if mErr := r.queue.MarkDelivered(ctx, p.DecisionID); mErr != nil {
				// Delivered but not marked: the next attempt is a ledger
				// replay (200), so nothing is recorded twice.
				r.log.Error("decision-log relay: marking a delivery failed", zap.String("decision_id", p.DecisionID), zap.Error(mErr))
			}
			if r.OnDelivered != nil {
				r.OnDelivered()
			}
		case errors.Is(err, ErrPermanent):
			r.log.Error("decision-log relay: the ledger refused a record for good; parked for an operator",
				zap.String("decision_id", p.DecisionID), zap.String("tenant_id", p.TenantID), zap.Error(err))
			if mErr := r.queue.MarkParked(ctx, p.DecisionID, err.Error()); mErr != nil {
				r.log.Error("decision-log relay: parking failed", zap.String("decision_id", p.DecisionID), zap.Error(mErr))
			}
			if r.OnFailed != nil {
				r.OnFailed(true)
			}
		default:
			next := r.now().Add(backoff(p.Attempts))
			r.log.Warn("decision-log relay: delivery failed, will retry",
				zap.String("decision_id", p.DecisionID), zap.Int("attempts", p.Attempts), zap.Time("next_attempt_at", next), zap.Error(err))
			if mErr := r.queue.MarkRetry(ctx, p.DecisionID, err.Error(), next); mErr != nil {
				r.log.Error("decision-log relay: recording a retry failed", zap.String("decision_id", p.DecisionID), zap.Error(mErr))
			}
			if r.OnFailed != nil {
				r.OnFailed(false)
			}
		}
	}
	return len(batch)
}
