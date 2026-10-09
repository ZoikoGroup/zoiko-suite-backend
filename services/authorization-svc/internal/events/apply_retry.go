package events

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// retryDelay is the wait before re-applying a message whose apply failed:
// doubling from one second, capped at thirty. A variable so tests need not wait.
var retryDelay = func(attempt int) time.Duration {
	d := time.Second << min(attempt, 5)
	return min(d, 30*time.Second)
}

// applyUntilDone runs handle on one message until it succeeds, and reports
// whether it did; false means ctx ended first.
//
// The offset is committed only after this returns true. Committing past a
// failed apply was how a suspension or a revocation arriving during a database
// outage was lost for good: Handle reported the failure, the loop committed
// anyway, and the principal kept full authority with nothing left to replay.
// Holding the partition instead means later events on it wait behind this one
// — that delay is visible and bounded by the outage, where the loss was
// neither. Handle returns an error only for a failed store write: malformed,
// unknown and tenantless messages return nil and are committed past, so a
// poison message cannot hold the partition here.
func applyUntilDone(ctx context.Context, log *zap.Logger, eventKey string, handle func() error) bool {
	for attempt := 0; ; attempt++ {
		err := handle()
		if err == nil {
			return true
		}
		delay := retryDelay(attempt)
		log.Error("event apply failed — offset NOT committed; retrying the same message",
			zap.String("event_id", eventKey),
			zap.Int("attempt", attempt+1),
			zap.Duration("retry_in", delay),
			zap.Error(err))
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
}
