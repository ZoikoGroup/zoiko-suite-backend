// Package expiry closes governed assignments whose end date has passed.
//
// authorization-svc enforces an assignment's effective_to at the instant: from
// then on no authorize call is granted through it. What it does not do is tell
// anyone. identity-context-svc frames a session's roles at resolve time and
// ends sessions only on iam.assignment.revoked, so without this sweep an
// assignment that ran out (an end date set at grant, or an effective-dated
// revoke) stayed PROVISIONED in this register and left every live session
// asserting the role until the session itself expired.
//
// The sweep finds PROVISIONED rows past their effective_to, closes each one
// (EXPIRED, or REVOKED when the end was scheduled by a revoke) and enqueues
// iam.assignment.revoked in the same transaction; the outbox relay delivers it.
package expiry

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/telemetry"
)

// Store is what the sweep needs.
type Store interface {
	ExpireDueAssignments(ctx context.Context, limit int) (int, error)
}

// Interval is how often the sweep runs; the bound on how late, after an
// assignment's end, sessions holding it are told to end.
const Interval = 30 * time.Second

// batch bounds one pass; a pass that fills it runs again at once.
const batch = 200

type Sweeper struct {
	store    Store
	metrics  *telemetry.Domain
	log      *zap.Logger
	interval time.Duration
}

func New(store Store, metrics *telemetry.Domain, log *zap.Logger) *Sweeper {
	return &Sweeper{store: store, metrics: metrics, log: log, interval: Interval}
}

// Run sweeps until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce closes every due assignment, a batch at a time, and returns how
// many it closed.
func (s *Sweeper) SweepOnce(ctx context.Context) int {
	total := 0
	for {
		n, err := s.store.ExpireDueAssignments(ctx, batch)
		total += n
		if n > 0 {
			s.metrics.AssignmentsExpired.Add(float64(n))
		}
		if err != nil {
			if ctx.Err() == nil {
				s.metrics.AssignmentExpiryFailures.Inc()
				s.log.Error("assignment expiry sweep failed", zap.Int("closed_before_failure", n), zap.Error(err))
			}
			return total
		}
		if n > 0 {
			s.log.Info("assignments expired", zap.Int("count", n))
		}
		if n < batch {
			return total
		}
	}
}
