// Package expiry ends delegations whose window has closed, in every tenant,
// without waiting for somebody to read the register.
//
// Expiry used to be lazy: ListDelegations, GetDelegation and RevokeDelegation
// each swept the tenant of the request passing through. That makes the end of
// an authority depend on somebody looking, and the register least likely to be
// looked at is the one whose lapsed authority persists longest. Worse, the
// sweep was tenant-scoped, so a tenant nobody read never expired anything at
// all and authority.expired was never published for it -- leaving
// identity-context-svc with no signal to end the delegate's session.
//
// The read-path sweep is still there and still correct: it guarantees a
// register read never shows a grant as ACTIVE past its window, which matters
// for a governance read. This loop is what makes the event timely and the
// coverage complete. Both write the same expired_at, so neither can disagree
// with the other about when an authority ended.
package expiry

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

// Expirer is the store surface the sweeper needs.
type Expirer interface {
	ExpireDueAllTenants(ctx context.Context, limit int) ([]domain.DelegationGrant, error)
	DueCount(ctx context.Context) (due int64, oldestOverdue time.Duration, err error)
}

const (
	// DefaultBatchSize bounds one pass. A register unswept for a long time, or
	// restored from backup, can have a large due backlog; taking it in one
	// statement would hold a write lock across the whole table while it ran.
	DefaultBatchSize = 500

	// DefaultInterval is the idle poll, and it is the headline number for this
	// service: it bounds how long an authority can outlive its own window
	// before authority.expired is published and the delegate's session ends.
	//
	// 30s rather than the relay's 250ms because the two bound different things.
	// The relay bounds delivery of an event that has already happened, where
	// the state change is committed and only the notice is outstanding. This
	// loop decides WHEN the state change happens, and every tick is a full
	// cross-tenant query -- so the cost is paid whether or not anything is due,
	// and 30s is already far below any window an operator would set by hand.
	DefaultInterval = 30 * time.Second
)

type Sweeper struct {
	store     Expirer
	metrics   *telemetry.Domain
	log       *zap.Logger
	batchSize int
	interval  time.Duration
}

func New(s Expirer, m *telemetry.Domain, log *zap.Logger) *Sweeper {
	return &Sweeper{
		store:     s,
		metrics:   m,
		log:       log,
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
	}
}

// WithInterval overrides the idle poll. For tests, and for a deployment that
// needs a tighter expiry bound than the default.
func (s *Sweeper) WithInterval(d time.Duration) *Sweeper {
	if d > 0 {
		s.interval = d
	}
	return s
}

// Run sweeps until ctx is cancelled.
//
// A pass that filled its batch loops again with no wait, for the same reason
// the relay does: a backlog is exactly the situation where sleeping between
// batches is wrong, and a fixed-tick version clears at batchSize/interval rows
// per second no matter how far behind it is.
func (s *Sweeper) Run(ctx context.Context) {
	s.log.Info("expiry sweeper started",
		zap.Int("batch_size", s.batchSize),
		zap.Duration("idle_interval", s.interval),
	)
	for {
		n, err := s.SweepOnce(ctx)
		if ctx.Err() != nil {
			s.log.Info("expiry sweeper stopped")
			return
		}
		if err != nil {
			s.log.Error("expiry sweep failed", zap.Error(err))
		}
		s.observeDue(ctx)
		if n == s.batchSize && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			s.log.Info("expiry sweeper stopped")
			return
		case <-time.After(s.interval):
		}
	}
}

// SweepOnce expires one batch. Returns how many grants were flipped.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	expired, err := s.store.ExpireDueAllTenants(ctx, s.batchSize)
	if err != nil {
		if s.metrics != nil {
			s.metrics.ExpirySweepFailures.Inc()
		}
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	if s.metrics != nil {
		s.metrics.Expiries.Add(float64(len(expired)))
		// Lateness, measured per grant: how long the authority outlived its own
		// window before this loop ended it. On a healthy service this is
		// bounded by the poll interval. It rising is the signal that the sweep
		// is falling behind, and it is not visible in the count of expiries --
		// a service expiring plenty of grants, all of them hours late, looks
		// busy and healthy by every other measure.
		now := time.Now().UTC()
		for _, d := range expired {
			s.metrics.ExpiryLatenessSeconds.Observe(now.Sub(d.EffectiveTo).Seconds())
		}
	}
	s.log.Info("expired delegations", zap.Int("count", len(expired)))
	return len(expired), nil
}

// observeDue refreshes the backlog gauges.
//
// Kept out of SweepOnce so the numbers are still reported on a pass where the
// sweep itself failed, which is the pass where a growing backlog matters most.
func (s *Sweeper) observeDue(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	due, oldest, err := s.store.DueCount(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("due-count query failed", zap.Error(err))
		}
		return
	}
	s.metrics.ExpiryDuePending.Set(float64(due))
	s.metrics.ExpiryOldestOverdueSeconds.Set(oldest.Seconds())
}
