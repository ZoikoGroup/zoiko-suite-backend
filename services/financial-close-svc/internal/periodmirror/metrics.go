package periodmirror

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the mirror's Prometheus counters:
//
//	close_period_mirror_total{command,outcome}            outcome = applied|skipped|failed
//	close_period_mirror_failures_total{command,reason}    reason  = the Reason* constants
type Metrics struct {
	total    *prometheus.CounterVec
	failures *prometheus.CounterVec
}

// NewMetrics builds the counters and registers them on reg (nil = unregistered,
// for tests and for callers that do not export them).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "close_period_mirror_total",
			Help: "REF-05 period mirror steps, by ACC-14 command and outcome (applied|skipped|failed).",
		}, []string{"command", "outcome"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "close_period_mirror_failures_total",
			Help: "REF-05 period mirror failures, by ACC-14 command and reason. The local operation is never affected.",
		}, []string{"command", "reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.total, m.failures)
	}
	return m
}

func (m *Metrics) observe(command, outcome, reason string) {
	m.total.WithLabelValues(command, outcome).Inc()
	if outcome == OutcomeFailed {
		m.failures.WithLabelValues(command, reason).Inc()
	}
}
