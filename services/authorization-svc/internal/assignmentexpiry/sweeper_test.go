package assignmentexpiry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/assignmentexpiry"
)

type fakeStore struct {
	results []int // one answer per call; then 0
	err     error
	calls   []int
}

func (f *fakeStore) SweepExpiredAssignments(_ context.Context, limit int) (int, error) {
	f.calls = append(f.calls, limit)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.calls) <= len(f.results) {
		return f.results[len(f.calls)-1], nil
	}
	return 0, nil
}

func TestSweep_RunsAgainWhileAPassIsFull(t *testing.T) {
	f := &fakeStore{results: []int{200, 200, 17}}
	got := assignmentexpiry.New(f, zap.NewNop()).Sweep(context.Background())
	if got != 417 {
		t.Errorf("announced %d, want 417", got)
	}
	if len(f.calls) != 3 {
		t.Errorf("%d passes, want 3: a full pass means there may be more, a short one means done", len(f.calls))
	}
}

func TestSweep_StopsOnTheFirstShortPass(t *testing.T) {
	f := &fakeStore{results: []int{5}}
	assignmentexpiry.New(f, zap.NewNop()).Sweep(context.Background())
	if len(f.calls) != 1 {
		t.Errorf("%d passes, want 1", len(f.calls))
	}
}

// A failure is not fatal and not a spin: the sweep ends for this tick, and the
// next tick retries the rows, which are still unannounced.
func TestSweep_AFailedPassEndsTheTickWithoutRetryingInALoop(t *testing.T) {
	f := &fakeStore{err: errors.New("db down")}
	got := assignmentexpiry.New(f, zap.NewNop()).Sweep(context.Background())
	if got != 0 || len(f.calls) != 1 {
		t.Errorf("announced %d over %d calls, want 0 over 1", got, len(f.calls))
	}
}

// Behind a producer that never lets a pass run short, one tick still ends.
func TestSweep_IsBoundedPerTick(t *testing.T) {
	f := &fakeStore{}
	for i := 0; i < 500; i++ {
		f.results = append(f.results, 200)
	}
	assignmentexpiry.New(f, zap.NewNop()).Sweep(context.Background())
	if len(f.calls) > 100 {
		t.Errorf("%d passes in one tick: unbounded", len(f.calls))
	}
}

func TestRun_StopsWhenTheContextIsDone(t *testing.T) {
	f := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { assignmentexpiry.New(f, zap.NewNop()).Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
	if len(f.calls) == 0 {
		t.Error("Run never swept: the first sweep must not wait a whole interval")
	}
}
