// Package boundary runs the subscription boundary worker: auto-renewals at
// term end, and the events for versions that take effect later than they
// were written.
package boundary

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Processor claims and processes one due boundary. It reports whether it
// found one; an error with found=true is a recorded item failure, an error
// with found=false means the queue itself could not be read.
type Processor interface {
	ProcessNextBoundary(ctx context.Context, now time.Time) (bool, error)
}

type Worker struct {
	p        Processor
	interval time.Duration
	batch    int
	logger   *zap.Logger
	now      func() time.Time
}

func NewWorker(p Processor, interval time.Duration, batch int, logger *zap.Logger) *Worker {
	return &Worker{p: p, interval: interval, batch: batch, logger: logger,
		now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

// Start runs until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce drains up to batch due boundaries and returns how many it took.
// An item failure is logged and the drain continues: the failed item has
// already been rescheduled with a backoff, so it cannot be picked again in
// this pass.
func (w *Worker) RunOnce(ctx context.Context) int {
	n := 0
	for n < w.batch && ctx.Err() == nil {
		found, err := w.p.ProcessNextBoundary(ctx, w.now())
		if err != nil {
			if !found {
				w.logger.Error("boundary queue unavailable", zap.Error(err))
				return n
			}
			w.logger.Error("boundary item failed; rescheduled", zap.Error(err))
		}
		if !found {
			return n
		}
		n++
	}
	return n
}
