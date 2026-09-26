package webhook_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/webhook"
)

type mockDLQProcessor struct {
	calls      int
	lastLimit  int
	returnVal  int
	returnErr  error
	calledChan chan struct{}
}

func (m *mockDLQProcessor) ProcessRetryableDLQ(_ context.Context, limit int) (int, error) {
	m.calls++
	m.lastLimit = limit
	if m.calledChan != nil {
		select {
		case m.calledChan <- struct{}{}:
		default:
		}
	}
	return m.returnVal, m.returnErr
}

func TestDLQWorker_RunOnce_Success(t *testing.T) {
	mock := &mockDLQProcessor{returnVal: 5}
	w := webhook.NewDLQWorker(mock, webhook.DLQWorkerOptions{
		Interval:  10 * time.Millisecond,
		BatchSize: 25,
	}, zap.NewNop())

	count, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 5, count)
	assert.Equal(t, 1, mock.calls)
	assert.Equal(t, 25, mock.lastLimit)
}

func TestDLQWorker_RunOnce_ErrorHandled(t *testing.T) {
	mock := &mockDLQProcessor{returnErr: errors.New("db connection failure")}
	w := webhook.NewDLQWorker(mock, webhook.DLQWorkerOptions{
		Interval:  10 * time.Millisecond,
		BatchSize: 10,
	}, zap.NewNop())

	count, err := w.RunOnce(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, count)
	assert.Equal(t, 1, mock.calls)
}

func TestDLQWorker_StartAndStopCleanly(t *testing.T) {
	calledChan := make(chan struct{}, 10)
	mock := &mockDLQProcessor{returnVal: 1, calledChan: calledChan}
	w := webhook.NewDLQWorker(mock, webhook.DLQWorkerOptions{
		Interval:  15 * time.Millisecond,
		BatchSize: 10,
	}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	// Wait for at least one tick
	select {
	case <-calledChan:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("worker did not execute tick within timeout")
	}

	// Cancel context to stop worker
	cancel()

	select {
	case <-done:
		// Clean exit
	case <-time.After(500 * time.Millisecond):
		t.Fatal("worker did not stop cleanly on context cancellation")
	}

	assert.GreaterOrEqual(t, mock.calls, 1)
}

func TestDLQWorker_OneFailedCycleDoesNotKillWorker(t *testing.T) {
	calledChan := make(chan struct{}, 10)
	// Return error on first call, then succeed
	mock := &mockDLQProcessor{returnErr: errors.New("transient error"), calledChan: calledChan}
	w := webhook.NewDLQWorker(mock, webhook.DLQWorkerOptions{
		Interval:  10 * time.Millisecond,
		BatchSize: 10,
	}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Start(ctx)

	// Wait for multiple ticks despite errors
	for i := 0; i < 2; i++ {
		select {
		case <-calledChan:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("worker died or stalled after failure on tick %d", i+1)
		}
	}

	assert.GreaterOrEqual(t, mock.calls, 2)
}
