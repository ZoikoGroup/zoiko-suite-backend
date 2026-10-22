// Package scheduler promotes SCHEDULED policy versions to ACTIVE when their
// effective time arrives (GOV-05 ScheduleActivation; V-001 PDC-I-27: the
// legal effective time, not the deployment time, is the activation instant).
//
// Resolution never waits for this loop — a scheduled version governs from
// its effective_from regardless — so a slow or stopped scheduler delays only
// the status flip and the policy.version.activated event, never a decision.
package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Promoter is the store method the scheduler drives.
type Promoter interface {
	PromoteDueActivations(ctx context.Context, limit int) (int, error)
}

const batch = 50

// Scheduler runs Promoter on an interval.
type Scheduler struct {
	p        Promoter
	interval time.Duration
	log      *zap.Logger
	stop     chan struct{}
	done     chan struct{}
}

// New builds a scheduler.
func New(p Promoter, interval time.Duration, log *zap.Logger) *Scheduler {
	return &Scheduler{p: p, interval: interval, log: log, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs the loop until Stop.
func (s *Scheduler) Start() {
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), s.interval+30*time.Second)
				n, err := s.p.PromoteDueActivations(ctx, batch)
				cancel()
				if err != nil {
					s.log.Error("scheduled activation pass failed", zap.Error(err))
				} else if n > 0 {
					s.log.Info("scheduled policy versions activated", zap.Int("count", n))
				}
			}
		}
	}()
}

// Stop ends the loop and waits for an in-flight pass.
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}
