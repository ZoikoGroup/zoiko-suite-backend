package expiry

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/telemetry"
)

type fakeStore struct {
	results []int
	errAt   int
	calls   int
}

func (f *fakeStore) ExpireDueAssignments(_ context.Context, limit int) (int, error) {
	i := f.calls
	f.calls++
	if f.errAt > 0 && f.calls == f.errAt {
		return 0, errors.New("db down")
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return 0, nil
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func newSweeper(st Store) (*Sweeper, *telemetry.Domain) {
	m := telemetry.NewDomainWith(prometheus.NewRegistry(), "access-control-svc")
	return New(st, m, zap.NewNop()), m
}

// A full batch means there may be more: the pass keeps going until a short one,
// so a backlog after downtime is cleared in one pass, not one batch per tick.
func TestSweepOnce_DrainsFullBatches(t *testing.T) {
	st := &fakeStore{results: []int{batch, batch, 7}}
	s, m := newSweeper(st)
	if got := s.SweepOnce(context.Background()); got != 2*batch+7 {
		t.Fatalf("closed %d, want %d", got, 2*batch+7)
	}
	if st.calls != 3 {
		t.Fatalf("store called %d times, want 3", st.calls)
	}
	if v := counterValue(t, m.AssignmentsExpired); v != float64(2*batch+7) {
		t.Fatalf("expired counter %v", v)
	}
}

func TestSweepOnce_FailureIsCountedNotSwallowed(t *testing.T) {
	st := &fakeStore{errAt: 1}
	s, m := newSweeper(st)
	s.SweepOnce(context.Background())
	if v := counterValue(t, m.AssignmentExpiryFailures); v != 1 {
		t.Fatalf("failure counter %v, want 1", v)
	}
}
