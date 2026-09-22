package expiry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

type fakeStore struct {
	mu sync.Mutex

	// batches is consumed one call at a time, so a test can describe a backlog
	// that takes several passes to clear.
	batches [][]domain.DelegationGrant
	calls   int
	err     error

	due           int64
	oldestOverdue time.Duration
	dueErr        error
}

func (f *fakeStore) ExpireDueAllTenants(_ context.Context, _ int) ([]domain.DelegationGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

func (f *fakeStore) DueCount(_ context.Context) (int64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.due, f.oldestOverdue, f.dueErr
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestSweeper(t *testing.T, s Expirer) (*Sweeper, *telemetry.Domain, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := telemetry.NewDomainWith(reg, "delegated-authority-svc")
	return New(s, m, zap.NewNop()), m, reg
}

// metricValue reads a single-valued collector out of the registry by name.
func metricValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// histogramSum reads a histogram's accumulated sum, which is what carries the
// lateness in seconds rather than the count of observations.
func histogramSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				return h.GetSampleSum()
			}
		}
	}
	return 0
}

func grant(id string, effectiveTo time.Time) domain.DelegationGrant {
	return domain.DelegationGrant{
		DelegationID: id,
		TenantID:     "t-1",
		EffectiveTo:  effectiveTo,
		Status:       domain.DelegationStatusExpired,
	}
}

// The central claim of this package: a delegation expires because its window
// closed, not because somebody read the register.
func TestSweepOnceExpiresWithoutAnyRead(t *testing.T) {
	past := time.Now().UTC().Add(-2 * time.Hour)
	f := &fakeStore{batches: [][]domain.DelegationGrant{{grant("d-1", past), grant("d-2", past)}}}
	s, _, reg := newTestSweeper(t, f)

	n, err := s.SweepOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, float64(2), metricValue(t, reg, "delegated_authority_expiries_total"))
}

// Lateness is the metric that distinguishes a sweeper doing its job from one
// expiring plenty of grants hours after the fact — which looks identical on
// every other signal.
func TestSweepRecordsLatenessPerGrant(t *testing.T) {
	past := time.Now().UTC().Add(-90 * time.Minute)
	f := &fakeStore{batches: [][]domain.DelegationGrant{{grant("d-1", past)}}}
	s, _, reg := newTestSweeper(t, f)

	_, err := s.SweepOnce(context.Background())
	require.NoError(t, err)

	require.Equal(t, float64(1), metricValue(t, reg, "delegated_authority_expiry_lateness_seconds"))
	sum := histogramSum(t, reg, "delegated_authority_expiry_lateness_seconds")
	require.InDelta(t, (90 * time.Minute).Seconds(), sum, 5,
		"lateness must be measured from effective_to, not from the sweep")
}

// A failing sweep must be visible. The read-path sweep logs and swallows its
// errors so it cannot fail the read it piggybacks on, which leaves this counter
// as the only place a persistently failing sweep surfaces.
func TestSweepFailureIsCounted(t *testing.T) {
	f := &fakeStore{err: errors.New("database is in recovery")}
	s, _, reg := newTestSweeper(t, f)

	n, err := s.SweepOnce(context.Background())
	require.Error(t, err)
	require.Zero(t, n)
	require.Equal(t, float64(1), metricValue(t, reg, "delegated_authority_expiry_sweep_failures_total"))
	require.Zero(t, metricValue(t, reg, "delegated_authority_expiries_total"),
		"a failed sweep must not report expiries")
}

// The backlog gauges have to be refreshed even on a pass where the sweep
// itself failed — that is precisely the pass where a growing backlog matters.
func TestObserveDueRunsAfterAFailedSweep(t *testing.T) {
	f := &fakeStore{err: errors.New("boom"), due: 17, oldestOverdue: 3 * time.Hour}
	s, _, reg := newTestSweeper(t, f)
	s.interval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	require.Equal(t, float64(17), metricValue(t, reg, "delegated_authority_expiry_due_pending"))
	require.InDelta(t, (3 * time.Hour).Seconds(),
		metricValue(t, reg, "delegated_authority_expiry_oldest_overdue_seconds"), 1)
	require.Greater(t, metricValue(t, reg, "delegated_authority_expiry_sweep_failures_total"), float64(0))
}

// A full batch means there is more behind it, so the loop must come straight
// back rather than sleeping out its idle interval. Without this a large backlog
// drains at batchSize/interval per second however far behind it is.
func TestFullBatchLoopsWithoutWaiting(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	full := make([]domain.DelegationGrant, DefaultBatchSize)
	for i := range full {
		full[i] = grant("d", past)
	}
	f := &fakeStore{batches: [][]domain.DelegationGrant{full, full, {grant("last", past)}}}
	s, _, _ := newTestSweeper(t, f)
	// An interval long enough that any sleep between batches would blow the
	// deadline: if the loop waited, only one pass could complete.
	s.interval = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	require.GreaterOrEqual(t, f.callCount(), 3,
		"a full batch must loop again immediately instead of sleeping")
}

// A short batch means the backlog is clear, so the loop must sleep rather than
// spin — otherwise an idle service runs a cross-tenant query as fast as the
// database will answer one.
func TestShortBatchSleeps(t *testing.T) {
	f := &fakeStore{}
	s, _, _ := newTestSweeper(t, f)
	s.interval = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	require.LessOrEqual(t, f.callCount(), 2,
		"an empty sweep must wait out the idle interval, not spin")
}

func TestWithIntervalIgnoresNonPositive(t *testing.T) {
	f := &fakeStore{}
	s, _, _ := newTestSweeper(t, f)
	require.Equal(t, DefaultInterval, s.WithInterval(0).interval,
		"zero must leave the default in place, not disable the sweeper")
	require.Equal(t, DefaultInterval, s.WithInterval(-time.Second).interval)
	require.Equal(t, 5*time.Second, s.WithInterval(5*time.Second).interval)
}

func TestRunStopsOnContextCancel(t *testing.T) {
	f := &fakeStore{}
	s, _, _ := newTestSweeper(t, f)
	s.interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
