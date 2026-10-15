// Package sweeps runs WFP-06's clock-driven work: a RECOGNIZED liability becomes DUE when its
// due date arrives. It writes through the store with the same events as the manual sweep.
package sweeps

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/store"
)

type Source interface {
	SweepDueAllTenants(ctx context.Context, asOf string, mk store.LiabilityEventBuilder) (int, error)
}

type DueSweeper struct {
	store    Source
	interval time.Duration
	now      func() time.Time
	log      *zap.Logger
}

func NewDueSweeper(st Source, interval time.Duration, log *zap.Logger) *DueSweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &DueSweeper{store: st, interval: interval, now: time.Now, log: log}
}

func (d *DueSweeper) Start(ctx context.Context) {
	d.log.Info("liability due sweep started", zap.Duration("interval", d.interval))
	t := time.NewTicker(d.interval)
	defer t.Stop()
	d.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			d.log.Info("liability due sweep stopped")
			return
		case <-t.C:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce moves every liability due on or before today (UTC) and returns how many moved.
func (d *DueSweeper) RunOnce(ctx context.Context) int {
	n, err := d.store.SweepDueAllTenants(ctx, d.now().UTC().Format("2006-01-02"), systemEvents())
	if err != nil && ctx.Err() == nil {
		d.log.Error("liability due sweep failed", zap.Error(err))
	}
	return n
}

func systemEvents() store.LiabilityEventBuilder {
	return func(ch store.LiabilityChange) ([]domain.OutboxEvent, error) {
		return events.BuildLiabilityEvents(ch.Kind, ch.Liability, ch.Exception, ch.Detail, "payroll-run-svc", "due-sweep", "")
	}
}
