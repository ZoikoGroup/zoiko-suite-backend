package retention_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/retention"
)

type fakeStore struct {
	mu sync.Mutex

	ensured   []string // months, as YYYY-MM
	ensureErr map[string]error

	detachCutoff []string // cutoffs, as YYYY-MM-DD
	detachResult []retention.Detached
	detachErr    error
	detachCalls  int

	lockGranted bool
	lockErr     error
	lockCalls   int
	unlockCalls int
}

func newFake() *fakeStore {
	return &fakeStore{lockGranted: true, ensureErr: map[string]error{}}
}

func (f *fakeStore) EnsureAccessDecisionPartition(_ context.Context, month time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := month.Format("2006-01")
	f.ensured = append(f.ensured, key)
	if err := f.ensureErr[key]; err != nil {
		return "", err
	}
	return "access_decision_log_" + month.Format("2006_01"), nil
}

func (f *fakeStore) DetachAccessDecisionPartitionsBefore(_ context.Context, cutoff time.Time) ([]retention.Detached, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachCalls++
	f.detachCutoff = append(f.detachCutoff, cutoff.Format("2006-01-02"))
	return f.detachResult, f.detachErr
}

func (f *fakeStore) TryRetentionLock(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lockCalls++
	return f.lockGranted, f.lockErr
}

func (f *fakeStore) ReleaseRetentionLock(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlockCalls++
	return nil
}

func (f *fakeStore) months() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ensured...)
}

// The runway must include the CURRENT month, not just future ones. The case
// that matters is a service starting into a month nobody pre-created, where
// every insert is already landing in the default partition — extending from
// "next month" would leave that unrepaired.
func TestSweep_EnsuresCurrentMonthAndTheRunway(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 3, 24)

	r.Sweep(context.Background())

	got := f.months()
	if len(got) != 4 {
		t.Fatalf("ensured %d months (%v), want 4 — the current month plus three ahead", len(got), got)
	}

	now := time.Now().UTC()
	wantFirst := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	if got[0] != wantFirst {
		t.Errorf("first month ensured was %s, want the current month %s", got[0], wantFirst)
	}

	// Consecutive, with no gap — a gap is a month of decisions in the default
	// partition.
	for i := 1; i < len(got); i++ {
		prev, _ := time.Parse("2006-01", got[i-1])
		want := prev.AddDate(0, 1, 0).Format("2006-01")
		if got[i] != want {
			t.Errorf("month %d was %s, want %s — the runway has a gap", i, got[i], want)
		}
	}
}

// A failure on one month must not abandon the rest. The near months are the
// ones that keep the service answering, and a failure on month+3 says nothing
// about month+1.
func TestSweep_OneMonthFailingDoesNotAbandonTheRest(t *testing.T) {
	f := newFake()
	now := time.Now().UTC()
	failing := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0).Format("2006-01")
	f.ensureErr[failing] = errors.New("permission denied for schema public")

	r := retention.New(f, zap.NewNop(), time.Hour, 3, 24)
	r.Sweep(context.Background())

	if len(f.months()) != 4 {
		t.Fatalf("ensured %v — a single month's failure stopped the loop", f.months())
	}
	// And the detach half still ran: the two halves are independent.
	if f.detachCalls != 1 {
		t.Errorf("detach ran %d times, want 1 — a create failure must not skip the detach", f.detachCalls)
	}
}

// The cutoff is the first of the month retentionMonths back, so a whole month
// has to have aged out before it is eligible.
func TestSweep_CutoffIsRetentionMonthsBack(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 1, 6)

	r.Sweep(context.Background())

	if len(f.detachCutoff) != 1 {
		t.Fatalf("detach called %d times, want 1", len(f.detachCutoff))
	}
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -6, 0).Format("2006-01-02")
	if f.detachCutoff[0] != want {
		t.Errorf("cutoff %s, want %s", f.detachCutoff[0], want)
	}
}

// 0 is the documented off switch on AUTHZ_ACCESS_DECISION_RETENTION_MONTHS.
// The runway must still be extended — disabling deletion is not disabling the
// half that keeps the service answering.
func TestSweep_RetentionZeroDisablesDetachingOnly(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 2, 0)

	r.Sweep(context.Background())

	if f.detachCalls != 0 {
		t.Errorf("detach ran with retention_months=0 — 0 is the off switch")
	}
	if len(f.months()) != 3 {
		t.Errorf("ensured %v — the runway must still be extended when detaching is off", f.months())
	}
}

// A negative window is a misconfiguration. The safe reading of one on this path
// is "do not detach", never "detach more".
func TestSweep_NegativeRetentionNeverDetaches(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 1, -12)

	r.Sweep(context.Background())

	if f.detachCalls != 0 {
		t.Fatal("a negative retention window detached something — it must be read as 'do not detach'")
	}
}

// Another replica holding the lock means skip, not queue. The work is
// idempotent and time-based, so the next tick covers anything missed — and
// blocking would stack goroutines behind a stuck sweep.
func TestSweep_SkipsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	f := newFake()
	f.lockGranted = false

	r := retention.New(f, zap.NewNop(), time.Hour, 3, 24)
	r.Sweep(context.Background())

	if len(f.months()) != 0 || f.detachCalls != 0 {
		t.Errorf("swept without the lock: months=%v detach=%d", f.months(), f.detachCalls)
	}
	if f.unlockCalls != 0 {
		t.Error("released a lock it never held")
	}
}

// A lock error must not be read as "lock acquired". Failing closed here means
// skipping a sweep, which costs one interval.
func TestSweep_LockErrorSkipsTheSweep(t *testing.T) {
	f := newFake()
	f.lockErr = errors.New("connection refused")

	r := retention.New(f, zap.NewNop(), time.Hour, 3, 24)
	r.Sweep(context.Background())

	if len(f.months()) != 0 || f.detachCalls != 0 {
		t.Error("swept despite being unable to take the lock")
	}
}

// The lock is always released, including when the sweep's own work fails —
// otherwise one bad sweep wedges every replica until the session ends.
func TestSweep_ReleasesTheLockAfterAFailedSweep(t *testing.T) {
	f := newFake()
	f.detachErr = errors.New("must be owner of table access_decision_log")
	now := time.Now().UTC()
	for i := 0; i <= 3; i++ {
		key := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0).Format("2006-01")
		f.ensureErr[key] = errors.New("permission denied")
	}

	r := retention.New(f, zap.NewNop(), time.Hour, 3, 24)
	r.Sweep(context.Background())

	if f.unlockCalls != 1 {
		t.Fatalf("unlock called %d times after a wholly failed sweep, want 1", f.unlockCalls)
	}
}

// monthsAhead below the floor is raised rather than honoured: a runway of zero
// means the partition for next month is never created ahead of time.
func TestNew_RaisesMonthsAheadBelowTheFloor(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 0, 24)

	r.Sweep(context.Background())

	if len(f.months()) < 2 {
		t.Fatalf("ensured %v — monthsAhead=0 was honoured, leaving no runway", f.months())
	}
}

// Run sweeps immediately rather than waiting for the first tick. A service
// starting after a long outage needs the runway now.
func TestRun_SweepsImmediately(t *testing.T) {
	f := newFake()
	r := retention.New(f, zap.NewNop(), time.Hour, 1, 24)

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if len(f.months()) > 0 {
			cancel()
			return
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not sweep before the first tick — a restart would wait a whole interval")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Detached partitions are reported, not swallowed: they have left the parent
// table but still hold decision artifacts, so somebody has to archive them.
func TestSweep_ReportsWhatItDetached(t *testing.T) {
	f := newFake()
	f.detachResult = []retention.Detached{
		{PartitionName: "access_decision_log_2024_01", RowCount: 12345},
		{PartitionName: "access_decision_log_2024_02", RowCount: 6789},
	}

	r := retention.New(f, zap.NewNop(), time.Hour, 1, 12)
	r.Sweep(context.Background()) // must not panic on a non-empty result

	if f.detachCalls != 1 {
		t.Fatalf("detach called %d times, want 1", f.detachCalls)
	}
}
