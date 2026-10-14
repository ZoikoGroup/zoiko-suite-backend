// Package accounting runs the WFP-05 dispatcher: it submits SUBMITTED payroll
// accounting subjects to general-ledger-svc's posting engine and records the
// outcome. It never writes the ledger itself; internal/ledger is the only path.
package accounting

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Poster submits one subject (internal/ledger.Client satisfies it).
type Poster interface {
	Post(ctx context.Context, b domain.AccountingBridge, originalJournalID string) store.PostOutcome
}

// Source is the store surface the dispatcher needs.
type Source interface {
	DispatchPending(ctx context.Context, limit int, post store.PostFunc, mk store.AccountingEventBuilder) (int, error)
}

type Dispatcher struct {
	store    Source
	poster   Poster
	interval time.Duration
	batch    int
	log      *zap.Logger
}

// New builds a dispatcher. A small batch keeps the transaction (which holds row
// locks while the ledger is called) short.
func New(st Source, poster Poster, interval time.Duration, batch int, log *zap.Logger) *Dispatcher {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 10
	}
	return &Dispatcher{store: st, poster: poster, interval: interval, batch: batch, log: log}
}

// Start dispatches until ctx ends.
func (d *Dispatcher) Start(ctx context.Context) {
	d.log.Info("payroll accounting dispatcher started", zap.Duration("interval", d.interval), zap.Int("batch", d.batch))
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("payroll accounting dispatcher stopped")
			return
		case <-t.C:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce dispatches one batch and returns how many subjects were POSTED.
func (d *Dispatcher) RunOnce(ctx context.Context) int {
	n, err := d.store.DispatchPending(ctx, d.batch, d.poster.Post, systemEvents())
	if err != nil && ctx.Err() == nil {
		d.log.Error("payroll accounting dispatch failed", zap.Error(err))
	}
	return n
}

// systemEvents builds the events the dispatcher writes. The actor is the service
// itself: no human principal caused the ledger's answer.
func systemEvents() store.AccountingEventBuilder {
	return func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		return events.BuildAccountingEvents(string(ch.Kind), ch.Bridge, ch.Original, ch.Reason, "payroll-run-svc", ch.Bridge.CorrelationID, "")
	}
}
