// Package notice runs the clock for regulated notices (ZS-SVC-Y-001 NCD-05).
//
// A notice's lifecycle depends on facts that arrive on their own schedule (a mail server's
// answer) and on deadlines that pass whether or not anyone is looking. Without a sweeper a
// notice would stay ACK_PENDING after its deadline until somebody happened to read it, and
// the "no response by deadline" exception (8.3) would never be raised. The sweeper only
// applies the same rule a read applies; it never invents a response.
package notice

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/store"
)

// Store is what the sweeper needs.
type Store interface {
	FindOpenNotices(ctx context.Context, limit int) ([]store.OpenNotice, error)
	RefreshNotice(ctx context.Context, noticeID string, now time.Time) (*domain.Notice, error)
}

// Worker refreshes open notices.
type Worker struct {
	store     Store
	interval  time.Duration
	batchSize int
	now       func() time.Time
	log       *zap.Logger
}

// NewWorker builds a sweeper. A non-positive interval or batch falls back to a safe default.
func NewWorker(s Store, interval time.Duration, batchSize int, log *zap.Logger) *Worker {
	if interval <= 0 {
		interval = time.Minute
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Worker{store: s, interval: interval, batchSize: batchSize, now: time.Now, log: log}
}

// RunOnce refreshes one batch and returns how many notices it processed.
func (w *Worker) RunOnce(ctx context.Context) int {
	open, err := w.store.FindOpenNotices(ctx, w.batchSize)
	if err != nil {
		w.log.Error("notice sweeper: could not list open notices", zap.Error(err))
		return 0
	}
	processed := 0
	for _, o := range open {
		tctx := svcmiddleware.WithTenant(ctx, o.TenantID)
		if _, err := w.store.RefreshNotice(tctx, o.NoticeID, w.now().UTC()); err != nil {
			w.log.Error("notice sweeper: refresh failed", zap.String("notice_id", o.NoticeID), zap.Error(err))
			continue
		}
		processed++
	}
	return processed
}

// Start runs until the context is cancelled.
func (w *Worker) Start(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.log.Info("notice sweeper started", zap.Duration("interval", w.interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.RunOnce(ctx)
		}
	}
}
