// Package ledgermetrics is the decision-log relay's health on /metrics
// (identity-envelope/decisionledger.Gauges; tracker "Assignment 25 Nov 2026").
// Alert on evidence_requirements_decision_log_outbox_oldest_pending_seconds (the ledger is
// behind) and on evidence_requirements_decision_log_outbox_parked > 0 (a record the ledger
// refused needs an operator).
package ledgermetrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/identity-envelope/decisionledger"
)

// Metrics implements decisionledger.Gauges.
type Metrics struct {
	pending    prometheus.Gauge
	parked     prometheus.Gauge
	oldest     prometheus.Gauge
	deliveries *prometheus.CounterVec
	purged     prometheus.Counter
}

// New registers the relay metrics on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		pending: prometheus.NewGauge(prometheus.GaugeOpts{Name: "evidence_requirements_decision_log_outbox_pending",
			Help: "Governed decisions not yet in governance-decision-log-svc."}),
		parked: prometheus.NewGauge(prometheus.GaugeOpts{Name: "evidence_requirements_decision_log_outbox_parked",
			Help: "Decision-log records the ledger refused permanently; each needs an operator."}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{Name: "evidence_requirements_decision_log_outbox_oldest_pending_seconds",
			Help: "Age of the oldest decision-log record not yet delivered (0 when none)."}),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "evidence_requirements_decision_log_relay_deliveries_total",
			Help: "Decision-log delivery attempts by result (delivered, retry, parked)."}, []string{"result"}),
		purged: prometheus.NewCounter(prometheus.CounterOpts{Name: "evidence_requirements_decision_log_outbox_purged_total",
			Help: "Delivered decision-log outbox rows purged after retention."}),
	}
	reg.MustRegister(m.pending, m.parked, m.oldest, m.deliveries, m.purged)
	return m
}

// SetBacklog implements decisionledger.Gauges.
func (m *Metrics) SetBacklog(b decisionledger.Backlog) {
	m.pending.Set(float64(b.Pending))
	m.parked.Set(float64(b.Parked))
	m.oldest.Set(b.OldestPending.Seconds())
}

// Delivered implements decisionledger.Gauges.
func (m *Metrics) Delivered() { m.deliveries.WithLabelValues("delivered").Inc() }

// Failed implements decisionledger.Gauges.
func (m *Metrics) Failed(parked bool) {
	if parked {
		m.deliveries.WithLabelValues("parked").Inc()
		return
	}
	m.deliveries.WithLabelValues("retry").Inc()
}

// Purged implements decisionledger.Gauges.
func (m *Metrics) Purged(n int64) { m.purged.Add(float64(n)) }
