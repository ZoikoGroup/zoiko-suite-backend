package telemetry

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestMetrics(t *testing.T, cache CacheStats) (*AuthzMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return NewAuthzMetrics(reg, "authorization-svc", cache), reg
}

func TestAuthzMetrics_DecisionCounters(t *testing.T) {
	m, _ := newTestMetrics(t, nil)
	m.ObserveDecision("DENY", "DENIED", "sod", "PAYMENT_RELEASE")
	m.ObserveDecision("DENY", "DENIED", "sod", "PAYMENT_RELEASE")
	m.ObserveDecision("PERMIT", "GRANTED", "rbac", "PAYMENT_RELEASE")
	if got := testutil.ToFloat64(m.decisions.WithLabelValues("DENY", "DENIED", "sod")); got != 2 {
		t.Fatalf("sod denials = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.decisionsAction.WithLabelValues("PAYMENT_RELEASE", "PERMIT")); got != 1 {
		t.Fatalf("permits by action = %v, want 1", got)
	}
	m.DecisionRecordFailed()
	if got := testutil.ToFloat64(m.recordFailures); got != 1 {
		t.Fatalf("record failures = %v, want 1", got)
	}
}

func TestAuthzMetrics_CacheFamiliesReadTheCache(t *testing.T) {
	m, reg := newTestMetrics(t, func() (uint64, uint64, uint64, int, time.Duration, bool) {
		return 7, 3, 2, 11, 1500 * time.Millisecond, true
	})
	_ = m
	want := `
# HELP authorization_cache_hits_total Evaluation reads answered from the cache.
# TYPE authorization_cache_hits_total counter
authorization_cache_hits_total{service="authorization-svc"} 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "authorization_cache_hits_total"); err != nil {
		t.Fatal(err)
	}
	if n, _ := testutil.GatherAndCount(reg, "authorization_cache_entries", "authorization_cache_misses_total", "authorization_cache_invalidations_total"); n != 3 {
		t.Fatalf("cache gauge/counter series = %d, want 3", n)
	}
}

func TestAuthzMetrics_ApplySnapshot(t *testing.T) {
	m, _ := newTestMetrics(t, nil)
	m.Apply(OperationalSnapshot{
		OutboxPending: 4, OutboxOldestPendingSeconds: 12.5, OutboxStuck: 1,
		Sessions:     map[string]map[string]int64{"break_glass": {"ACTIVE": 1, "PENDING_APPROVAL": 2}},
		ExpiringSoon: map[string]int64{"jit": 3},
	})
	if testutil.ToFloat64(m.outboxPending) != 4 || testutil.ToFloat64(m.outboxStuck) != 1 || testutil.ToFloat64(m.outboxOldest) != 12.5 {
		t.Fatal("outbox gauges not applied")
	}
	if testutil.ToFloat64(m.sessions.WithLabelValues("break_glass", "PENDING_APPROVAL")) != 2 {
		t.Fatal("break-glass pending not applied")
	}
	// A kind the snapshot did not mention reads zero, not a stale value.
	if testutil.ToFloat64(m.sessions.WithLabelValues("support", "ACTIVE")) != 0 {
		t.Fatal("support active should be 0")
	}
	if testutil.ToFloat64(m.sessionsExpiring.WithLabelValues("jit")) != 3 {
		t.Fatal("expiring-soon not applied")
	}
}

func TestAuthzMetrics_ProjectionLag(t *testing.T) {
	m, reg := newTestMetrics(t, nil)
	m.ObserveProjection("lifecycle", "principal.status.changed", 800*time.Millisecond, true)
	m.ObserveProjection("lifecycle", "role.updated", 0, false)
	if got := testutil.ToFloat64(m.projectionEvents.WithLabelValues("lifecycle", "role.updated")); got != 1 {
		t.Fatalf("projection events = %v, want 1", got)
	}
	// Both consumers' lag series exist from boot; only the event with a
	// timestamp contributed a sample.
	if n, _ := testutil.GatherAndCount(reg, "authorization_projection_lag_seconds"); n != 2 {
		t.Fatalf("lag series = %d, want 2 (delegation, lifecycle)", n)
	}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "authorization_projection_lag_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			c := m.GetHistogram().GetSampleCount()
			for _, l := range m.GetLabel() {
				if l.GetName() == "consumer" && ((l.GetValue() == "lifecycle" && c != 1) || (l.GetValue() == "delegation" && c != 0)) {
					t.Fatalf("%s lag count = %d", l.GetValue(), c)
				}
			}
		}
	}
}
