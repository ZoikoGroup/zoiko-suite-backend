package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestOutboxMetricsReport — audit X3: "no metric exists to alert on it". The
// OutboxWorker must be observable: a publish-failure counter and an outbox
// depth gauge, both labeled with the service, on the default registry that
// /metrics serves.
func TestOutboxMetricsReport(t *testing.T) {
	m := NewMetrics("jurisdiction-rules-svc-test")
	m.OutboxPublishFailuresTotal.WithLabelValues("jurisdiction.rule.updated").Inc()
	m.OutboxPendingEvents.Set(7)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("unexpected gather error: %v", err)
	}
	got := map[string]float64{}
	for _, mf := range families {
		for _, mm := range mf.GetMetric() {
			switch mf.GetName() {
			case "outbox_publish_failures_total":
				for _, l := range mm.GetLabel() {
					if l.GetName() == "event_type" {
						got["failures:"+l.GetValue()] = mm.GetCounter().GetValue()
					}
				}
			case "outbox_pending_events":
				got["pending"] = mm.GetGauge().GetValue()
			}
		}
	}
	if got["failures:jurisdiction.rule.updated"] != 1 {
		t.Errorf("outbox_publish_failures_total = %v, want 1 for jurisdiction.rule.updated", got["failures:jurisdiction.rule.updated"])
	}
	if got["pending"] != 7 {
		t.Errorf("outbox_pending_events = %v, want 7", got["pending"])
	}
}