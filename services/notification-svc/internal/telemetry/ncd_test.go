package telemetry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"zoiko.io/notification-svc/internal/ncd"
)

func TestNCDMetricsRecordAndCarryNoPersonalLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewNCD("notification-svc", reg)

	m.AttemptSubmitted("EMAIL", "smtp-primary", "UNKNOWN", 2*time.Second)
	m.Callback("smtp-primary", "rejected_invalid_signature")
	m.Callback("smtp-primary", "duplicate")
	m.Backlog(ncd.Backlog{UnknownAttempts: 3, OldestUnknown: 90 * time.Second, QueuedJobs: 7, RecordDeclarationsPending: 1,
		OpenExceptions: map[string]int{"UNKNOWN_UNRESOLVED": 2, "MISDELIVERY_INCIDENT": 1}, OldestOpenException: time.Minute})

	value := func(c prometheus.Collector) float64 {
		t.Helper()
		ch := make(chan prometheus.Metric, 1)
		c.Collect(ch)
		var out dto.Metric
		if err := (<-ch).Write(&out); err != nil {
			t.Fatal(err)
		}
		if out.Counter != nil {
			return out.Counter.GetValue()
		}
		return out.Gauge.GetValue()
	}
	if got := value(m.AttemptsTotal.WithLabelValues("EMAIL", "smtp-primary", "UNKNOWN")); got != 1 {
		t.Errorf("attempts = %v", got)
	}
	if got := value(m.CallbacksTotal.WithLabelValues("smtp-primary", "rejected_invalid_signature")); got != 1 {
		t.Errorf("rejected callbacks = %v", got)
	}
	if got := value(m.UnknownOldestAgeSeconds); got != 90 {
		t.Errorf("unknown oldest age = %v", got)
	}
	if got := value(m.QueuedJobs); got != 7 {
		t.Errorf("queued jobs = %v", got)
	}
	if got := value(m.OpenExceptions.WithLabelValues("UNKNOWN_UNRESOLVED")); got != 2 {
		t.Errorf("open UNKNOWN_UNRESOLVED exceptions = %v", got)
	}
	if got := value(m.OpenExceptionOldestAge); got != 60 {
		t.Errorf("oldest open exception age = %v", got)
	}
	// A kind whose exceptions were all resolved must drop to 0, not hold
	// its last count: a stale gauge is an alert that never clears.
	m.Backlog(ncd.Backlog{OpenExceptions: map[string]int{"UNKNOWN_UNRESOLVED": 1}})
	if got := value(m.OpenExceptions.WithLabelValues("MISDELIVERY_INCIDENT")); got != 0 {
		t.Errorf("resolved MISDELIVERY_INCIDENT should read 0, got %v", got)
	}

	// §13.3: no label may carry a tenant, address, principal or record id.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"service": true, "channel": true, "binding": true, "state": true, "outcome": true, "kind": true}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s carries label %q, which is not on the §13.3 allow-list", f.GetName(), l.GetName())
				}
			}
		}
	}
}

func TestSenderAuthGaugeFollowsTheMonitor(t *testing.T) {
	reg := prometheus.NewRegistry()
	set := RegisterSenderAuth("notification-svc", reg)
	read := func() float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil || len(families) != 1 {
			t.Fatalf("gather: %v (%d families)", err, len(families))
		}
		return families[0].GetMetric()[0].GetGauge().GetValue()
	}
	set(false)
	if got := read(); got != 0 {
		t.Errorf("held: gauge = %v, want 0", got)
	}
	set(true)
	if got := read(); got != 1 {
		t.Errorf("restored: gauge = %v, want 1", got)
	}
}
