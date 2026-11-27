package telemetry

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// gauge reads one gauge series (by name) from the default registry.
func gauge(t *testing.T, name string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() == name && len(f.GetMetric()) > 0 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no gauge %s", name)
	return 0
}

func has(t *testing.T, name string) bool {
	t.Helper()
	fams, _ := prometheus.DefaultGatherer.Gather()
	for _, f := range fams {
		if f.GetName() == name {
			return true
		}
	}
	return false
}

// One instance for the file: the metrics register on the process-wide default registry, as in production.
var (
	cwOnce sync.Once
	cw     *CertWatchMetrics
	clock  = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
)

func certMetrics() *CertWatchMetrics {
	cwOnce.Do(func() {
		cw = NewCertWatchMetrics("general-ledger-svc")
		cw.now = func() time.Time { return clock }
	})
	return cw
}

func TestCertWatchMetrics_TheScheduleIsReportedOnAndOffHonestly(t *testing.T) {
	m := certMetrics()
	m.SweepScheduled(15 * time.Minute)
	if got := gauge(t, "gl_cert_sweep_interval_seconds"); got != 900 {
		t.Fatalf("interval = %v", got)
	}
	// Off is reported as 0, so an alert on a stalled schedule can tell "off" from "stopped".
	m.SweepScheduled(0)
	if got := gauge(t, "gl_cert_sweep_interval_seconds"); got != 0 {
		t.Fatalf("interval when off = %v", got)
	}
}

func TestCertWatchMetrics_OnlyAFullySuccessfulPassCountsAsASuccess(t *testing.T) {
	m := certMetrics()
	base := counter(t, "gl_cert_sweep_runs_total", map[string]string{"outcome": "ok"})

	clock = clock.Add(time.Minute)
	m.SweepRun(certwatch.RunOK, 3*time.Second)
	okAt := float64(clock.Unix())
	if gauge(t, "gl_cert_sweep_last_success_timestamp_seconds") != okAt || gauge(t, "gl_cert_sweep_last_run_timestamp_seconds") != okAt {
		t.Fatal("an ok pass must set both timestamps")
	}

	// A partial pass completed but is NOT a success: the success timestamp stays where it was, so the stale-success alert keeps counting.
	clock = clock.Add(time.Minute)
	m.SweepRun(certwatch.RunPartial, time.Second)
	if gauge(t, "gl_cert_sweep_last_success_timestamp_seconds") != okAt {
		t.Fatal("a partial pass was recorded as a success")
	}
	if gauge(t, "gl_cert_sweep_last_run_timestamp_seconds") != float64(clock.Unix()) {
		t.Fatal("a partial pass is a completed pass")
	}

	// A pass that found the lock held, or could not run at all, completed nothing: neither timestamp moves, but each is counted.
	last := clock
	clock = clock.Add(time.Hour)
	m.SweepRun(certwatch.RunLocked, 0)
	m.SweepRun(certwatch.RunError, time.Second)
	if gauge(t, "gl_cert_sweep_last_run_timestamp_seconds") != float64(last.Unix()) || gauge(t, "gl_cert_sweep_last_success_timestamp_seconds") != okAt {
		t.Fatal("a locked or errored pass moved a timestamp")
	}
	for outcome, want := range map[string]float64{"ok": 1, "partial": 1, "locked": 1, "error": 1} {
		got := counter(t, "gl_cert_sweep_runs_total", map[string]string{"outcome": outcome})
		if outcome == "ok" {
			got -= base
		}
		if got != want {
			t.Fatalf("runs{outcome=%s} = %v, want %v", outcome, got, want)
		}
	}
	// The duration is observed for passes that ran, not for the one that found the lock held or could not run.
	if n := counter(t, "gl_cert_sweep_duration_seconds", nil); n != 2 {
		t.Fatalf("durations observed = %v, want 2 (ok and partial)", n)
	}
}

func TestCertWatchMetrics_ScopesUncheckedFlagsEventsAndConsumerErrors(t *testing.T) {
	m := certMetrics()
	read := func(name string, l map[string]string) float64 { return counter(t, name, l) }
	b := map[string]float64{
		"swept":  read("gl_cert_sweep_scopes_total", map[string]string{"result": "swept"}),
		"unauth": read("gl_cert_sweep_scopes_total", map[string]string{"result": "unauthorized"}),
		"error":  read("gl_cert_sweep_scopes_total", map[string]string{"result": "error"}),
		"unchk":  read("gl_cert_sweep_certifications_unchecked_total", nil),
	}
	m.SweepScope(certwatch.ScopeSwept)
	m.SweepScope(certwatch.ScopeUnauthorized)
	m.SweepScope(certwatch.ScopeUnauthorized)
	m.SweepScope(certwatch.ScopeError)
	m.CertificationsUnchecked(3)
	m.CertificationsUnchecked(0) // nothing to add
	for name, want := range map[string]float64{"swept": 1, "unauth": 2, "error": 1, "unchk": 3} {
		var got float64
		switch name {
		case "swept":
			got = read("gl_cert_sweep_scopes_total", map[string]string{"result": "swept"})
		case "unauth":
			got = read("gl_cert_sweep_scopes_total", map[string]string{"result": "unauthorized"})
		case "error":
			got = read("gl_cert_sweep_scopes_total", map[string]string{"result": "error"})
		case "unchk":
			got = read("gl_cert_sweep_certifications_unchecked_total", nil)
		}
		if got-b[name] != want {
			t.Fatalf("%s: +%v, want +%v", name, got-b[name], want)
		}
	}

	// Flags are counted by route, and a pass that raised none adds nothing.
	f0 := read("gl_cert_flags_raised_total", map[string]string{"trigger": "event"})
	m.FlagsRaised(certwatch.TriggerEvent, 4)
	m.FlagsRaised(certwatch.TriggerEvent, 0)
	m.FlagsRaised(certwatch.TriggerSchedule, 1)
	m.FlagsRaised(certwatch.TriggerManual, 2)
	if got := read("gl_cert_flags_raised_total", map[string]string{"trigger": "event"}) - f0; got != 4 {
		t.Fatalf("event flags +%v", got)
	}
	if read("gl_cert_flags_raised_total", map[string]string{"trigger": "schedule"}) != 1 || read("gl_cert_flags_raised_total", map[string]string{"trigger": "manual"}) != 2 {
		t.Fatal("schedule/manual flags")
	}

	for _, r := range []string{certwatch.EventHandled, certwatch.EventHandled, certwatch.EventIrrelevant, certwatch.EventMalformed, certwatch.EventFailed} {
		m.WithdrawalEvent(r)
	}
	for _, r := range []string{certwatch.ReactionSwept, certwatch.ReactionUnauthorized, certwatch.ReactionUnwatched, certwatch.ReactionFailed} {
		m.WithdrawalReaction(r)
	}
	m.ConsumerError(certwatch.ErrRead)
	m.ConsumerError(certwatch.ErrRead)
	m.ConsumerError(certwatch.ErrCommit)
	if read("gl_cert_withdrawal_events_total", map[string]string{"result": "handled"}) != 2 ||
		read("gl_cert_withdrawal_events_total", map[string]string{"result": "failed"}) != 1 ||
		read("gl_cert_withdrawal_events_total", map[string]string{"result": "malformed"}) != 1 ||
		read("gl_cert_withdrawal_reactions_total", map[string]string{"result": "unwatched"}) != 1 ||
		read("gl_cert_consumer_errors_total", map[string]string{"kind": "read"}) != 2 ||
		read("gl_cert_consumer_errors_total", map[string]string{"kind": "commit"}) != 1 {
		t.Fatal("event, reaction or consumer counters")
	}
}

func TestRegisterFlagGauges_ReadAtScrapeTimeAndNaNWhenTheReadFails(t *testing.T) {
	var mu sync.Mutex
	stats := certwatch.OpenFlagStats{Open: 7, Oldest: 90 * time.Minute}
	var failure error
	var reported []error
	RegisterFlagGauges("general-ledger-svc", func(context.Context) (certwatch.OpenFlagStats, error) {
		mu.Lock()
		defer mu.Unlock()
		return stats, failure
	}, func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() })

	if gauge(t, "gl_cert_flags_open") != 7 || gauge(t, "gl_cert_flags_oldest_open_age_seconds") != 5400 {
		t.Fatalf("open %v oldest %v", gauge(t, "gl_cert_flags_open"), gauge(t, "gl_cert_flags_oldest_open_age_seconds"))
	}
	// Read at scrape time: the next scrape sees the new state without anything pushing it.
	mu.Lock()
	stats = certwatch.OpenFlagStats{}
	mu.Unlock()
	if gauge(t, "gl_cert_flags_open") != 0 || gauge(t, "gl_cert_flags_oldest_open_age_seconds") != 0 {
		t.Fatal("not read at scrape time")
	}
	// A failed read is NaN, never a reassuring zero, and is reported.
	mu.Lock()
	failure = errors.New("db down")
	mu.Unlock()
	if v := gauge(t, "gl_cert_flags_open"); !math.IsNaN(v) {
		t.Fatalf("a failed read reported %v, want NaN", v)
	}
	mu.Lock()
	n := len(reported)
	mu.Unlock()
	if n == 0 {
		t.Fatal("a failed read was not reported")
	}
	_ = has
}

// No label carries data: the label names are a closed, tenant-free set, so cardinality cannot grow with tenants or certifications.
func TestCertWatchMetrics_LabelsAreClosedSetsNeverData(t *testing.T) {
	certMetrics()
	allowed := map[string]bool{"service": true, "outcome": true, "result": true, "trigger": true, "kind": true}
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range fams {
		if len(f.GetName()) < 8 || f.GetName()[:8] != "gl_cert_" {
			continue
		}
		seen++
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Fatalf("%s carries label %q", f.GetName(), l.GetName())
				}
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d certification metrics are registered", seen)
	}
}
