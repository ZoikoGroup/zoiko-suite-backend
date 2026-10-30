package telemetry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// KernelMetrics are the Global Accounting Kernel's operational metrics (standard §38): accepted and rejected events, posting
// latency, exception rate, duplicate-prevention hits, imbalance failures and reconciliation breaks. It satisfies kernel.Observer.
//
// Counters carry the accounting outcome, not just an HTTP status, so an alert can tell an accounting-control failure (a refused
// posting, a break) from an infrastructure failure (a dependency that is down).
type KernelMetrics struct {
	commands   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	exceptions *prometheus.CounterVec
	recon      *prometheus.CounterVec
}

// NewKernelMetrics registers the kernel metrics on the default registry.
func NewKernelMetrics(serviceName string) *KernelMetrics {
	labels := prometheus.Labels{"service": serviceName}
	m := &KernelMetrics{
		commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gl_kernel_commands_total", ConstLabels: labels,
			Help: "Kernel commands by outcome: ok, existing (a duplicate-prevention hit), refused (an accounting control said no), dependency, error.",
		}, []string{"command", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gl_kernel_command_duration_seconds", ConstLabels: labels, Help: "Kernel command latency in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"command"}),
		exceptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gl_kernel_exceptions_total", ConstLabels: labels,
			Help: "Posting-taxonomy exceptions (AK-AUTH, AK-PERIOD, AK-RULE, AK-COA, AK-DIM, AK-BAL, AK-FX, AK-TAX, AK-DUP, AK-CONC, AK-EXT, AK-DATA) by family and code.",
		}, []string{"family", "code"}),
		recon: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gl_kernel_reconciliation_runs_total", ConstLabels: labels,
			Help: "Reconciliation runs by type and the status they ended in (MATCHED, WARNING, BREAK).",
		}, []string{"type", "status"}),
	}
	prometheus.MustRegister(m.commands, m.duration, m.exceptions, m.recon)
	return m
}

// Command implements kernel.Observer.
func (m *KernelMetrics) Command(name, outcome string, d time.Duration) {
	m.commands.WithLabelValues(name, outcome).Inc()
	m.duration.WithLabelValues(name).Observe(d.Seconds())
}

// Exception implements kernel.Observer.
func (m *KernelMetrics) Exception(family, code string) {
	m.exceptions.WithLabelValues(family, code).Inc()
}

// ReconciliationResult implements kernel.Observer.
func (m *KernelMetrics) ReconciliationResult(reconType, status string) {
	m.recon.WithLabelValues(reconType, status).Inc()
}
