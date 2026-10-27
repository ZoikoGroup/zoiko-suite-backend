package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func instantRetry(t *testing.T) {
	t.Helper()
	saved := retryDelay
	retryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { retryDelay = saved })
}

// A failed apply is retried on the same message until it lands; only then may
// Run commit the offset.
func TestApplyUntilDone_RetriesUntilApplied(t *testing.T) {
	instantRetry(t)
	calls := 0
	ok := applyUntilDone(context.Background(), zap.NewNop(), "evt-1", func() error {
		calls++
		if calls < 3 {
			return errors.New("db down")
		}
		return nil
	})
	if !ok || calls != 3 {
		t.Fatalf("applyUntilDone = %v after %d calls, want true after 3", ok, calls)
	}
}

// Shutdown during an outage reports false, so Run returns WITHOUT committing
// and the message is redelivered to the next consumer.
func TestApplyUntilDone_StopsOnCancelWithoutSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	saved := retryDelay
	retryDelay = func(int) time.Duration { cancel(); return time.Hour }
	t.Cleanup(func() { retryDelay = saved })
	if applyUntilDone(ctx, zap.NewNop(), "evt-1", func() error { return errors.New("db down") }) {
		t.Fatal("applyUntilDone reported success for a message that never applied")
	}
}

func TestRetryDelay_Capped(t *testing.T) {
	if d := retryDelay(0); d != time.Second {
		t.Errorf("first delay = %v, want 1s", d)
	}
	if d := retryDelay(50); d != 30*time.Second {
		t.Errorf("delay after many attempts = %v, want the 30s cap", d)
	}
}
