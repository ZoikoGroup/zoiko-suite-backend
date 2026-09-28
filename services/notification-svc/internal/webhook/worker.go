package webhook

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// DLQProcessor defines the interface for reprocessing retryable webhook DLQ items.
// Satisfied by *Processor.
type DLQProcessor interface {
	ProcessRetryableDLQ(ctx context.Context, limit int) (int, error)
}

// DLQWorkerOptions configures the Webhook DLQ reprocessor worker.
type DLQWorkerOptions struct {
	Interval  time.Duration
	BatchSize int
}

// DLQWorker periodically polls and reprocesses eligible retryable DLQ records.
type DLQWorker struct {
	processor DLQProcessor
	opts      DLQWorkerOptions
	log       *zap.Logger
}

// NewDLQWorker initializes a new DLQWorker with safe defaults.
func NewDLQWorker(processor DLQProcessor, opts DLQWorkerOptions, log *zap.Logger) *DLQWorker {
	if opts.Interval <= 0 {
		opts.Interval = 1 * time.Minute
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &DLQWorker{
		processor: processor,
		opts:      opts,
		log:       log,
	}
}

// Start runs the background polling loop until ctx is cancelled.
// An error during any individual cycle is logged and does not terminate the worker.
func (w *DLQWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	w.log.Info("webhook dlq reprocessor worker started",
		zap.Duration("interval", w.opts.Interval),
		zap.Int("batch_size", w.opts.BatchSize),
	)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("webhook dlq reprocessor worker stopped")
			return
		case <-ticker.C:
			count, err := w.RunOnce(ctx)
			if err != nil {
				w.log.Error("webhook dlq reprocessing cycle encountered error", zap.Error(err))
			} else if count > 0 {
				w.log.Info("webhook dlq reprocessed items", zap.Int("reprocessed_count", count))
			}
		}
	}
}

// RunOnce executes a single batch reprocessing cycle.
func (w *DLQWorker) RunOnce(ctx context.Context) (int, error) {
	return w.processor.ProcessRetryableDLQ(ctx, w.opts.BatchSize)
}
