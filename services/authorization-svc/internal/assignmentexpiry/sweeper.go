// Package assignmentexpiry announces role assignments whose effective window has
// closed (ORG-06 AssignmentExpired, iam.assignment.expired).
//
// authorization-svc stops granting through an assignment the instant its
// effective_to passes. What it did not do was tell anyone, so a consumer with
// its own projection of who holds which role learned of the lapse late or not
// at all. The sweep closes that: it asks the database to announce each lapsed
// assignment once (migration 000038) and the outbox relay delivers the event.
package assignmentexpiry

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Store is what the sweep needs.
type Store interface {
	// SweepExpiredAssignments announces up to limit lapsed assignments and
	// returns how many it announced.
	SweepExpiredAssignments(ctx context.Context, limit int) (int, error)
}

// Interval is how often the sweep runs: the bound on how late, after an
// assignment's window closes, the world is told.
const Interval = 30 * time.Second

// batch bounds one pass; a pass that fills it runs again at once.
const batch = 200

// maxPasses stops one tick from looping forever behind a producer that outpaces
// it; the next tick carries on.
const maxPasses = 50

type Sweeper struct {
	store    Store
	log      *zap.Logger
	interval time.Duration
}

func New(s Store, log *zap.Logger) *Sweeper {
	return &Sweeper{store: s, log: log, interval: Interval}
}

// Run sweeps until ctx is done.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep runs passes until one is not full. A failed pass is logged and ends the
// sweep for this tick: the rows are still unannounced, so the next tick retries
// them, and nothing is lost by waiting.
func (s *Sweeper) Sweep(ctx context.Context) int {
	total := 0
	for pass := 0; pass < maxPasses; pass++ {
		n, err := s.store.SweepExpiredAssignments(ctx, batch)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("assignment expiry sweep failed — will retry next tick", zap.Error(err))
			}
			return total
		}
		total += n
		if n < batch {
			break
		}
	}
	if total > 0 {
		s.log.Info("assignments expired", zap.Int("announced", total))
	}
	return total
}
