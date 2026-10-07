package boundary

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

type scripted struct {
	results []struct {
		found bool
		err   error
	}
	calls int
}

func (s *scripted) ProcessNextBoundary(context.Context, time.Time) (bool, error) {
	if s.calls >= len(s.results) {
		s.calls++
		return false, nil
	}
	r := s.results[s.calls]
	s.calls++
	return r.found, r.err
}

func step(found bool, err error) struct {
	found bool
	err   error
} {
	return struct {
		found bool
		err   error
	}{found, err}
}

// An item failure does not stop the drain; an empty queue or an unreadable
// queue does; the batch caps a single pass.
func TestRunOnce(t *testing.T) {
	logger := zap.NewNop()
	p := &scripted{results: []struct {
		found bool
		err   error
	}{step(true, nil), step(true, errors.New("item failed")), step(true, nil), step(false, nil)}}
	if n := NewWorker(p, time.Minute, 10, logger).RunOnce(context.Background()); n != 3 || p.calls != 4 {
		t.Fatalf("drain: processed %d in %d calls, want 3 in 4", n, p.calls)
	}

	down := &scripted{results: []struct {
		found bool
		err   error
	}{step(true, nil), step(false, errors.New("connection refused")), step(true, nil)}}
	if n := NewWorker(down, time.Minute, 10, logger).RunOnce(context.Background()); n != 1 || down.calls != 2 {
		t.Fatalf("an unreadable queue must stop the pass: processed %d in %d calls", n, down.calls)
	}

	busy := &scripted{}
	for i := 0; i < 20; i++ {
		busy.results = append(busy.results, step(true, nil))
	}
	if n := NewWorker(busy, time.Minute, 5, logger).RunOnce(context.Background()); n != 5 {
		t.Fatalf("batch cap: processed %d, want 5", n)
	}
}
