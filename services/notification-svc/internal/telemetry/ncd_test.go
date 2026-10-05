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
	m.Backlog(ncd.Backlog{UnknownAttempts: 3, OldestUnknown: 90 * time.Second, QueuedJobs: 7, RecordDeclarationsPending: 1})

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

	// §13.3: no label may carry a tenant, address, principal or record id.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"service": true, "channel": true, "binding": true, "state": true, "outcome": true}
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
