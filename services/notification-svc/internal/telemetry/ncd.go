package telemetry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/notification-svc/internal/ncd"
)

// NCD holds the ZS-SVC-Y-001 control plane's operational metrics (§13.1).
//
// Before this the plane exported nothing: the legacy path was instrumented and
// the plane that now carries governed communications was not, so a stuck
// UNKNOWN attempt, a queue that had stopped draining, a flood of forged
// callbacks or a regulated notice past its deadline were visible only to
// someone reading the database.
//
// Two kinds of series, chosen by where the truth is:
//
//   - Counters for things that happen outside any transaction and so cannot
//     be rolled back: a provider submission's outcome and latency, and each
//     provider callback's fate.
//   - Gauges for backlogs, re-read from the database by the worker (Backlog).
//     Counting those in code would drift every time a transaction rolled back
//     and reset on every restart; the database is right by definition.
//
// No label carries a tenant, address, principal or communication id (§13.3).
// "binding" is platform configuration — a handful of values.
type NCD struct {
	AttemptsTotal  *prometheus.CounterVec
	SubmitDuration *prometheus.HistogramVec
	CallbacksTotal *prometheus.CounterVec

	UnknownAttempts           prometheus.Gauge
	UnknownOldestAgeSeconds   prometheus.Gauge
	QueuedJobs                prometheus.Gauge
	QueuedOldestAgeSeconds    prometheus.Gauge
	NoticesPastDeadline       prometheus.Gauge
	RecordDeclarationsPending prometheus.Gauge
	RecordPendingOldestAge    prometheus.Gauge
	OpenExceptions            *prometheus.GaugeVec
	OpenExceptionOldestAge    prometheus.Gauge

	// seenKinds remembers every exception kind ever reported, so a kind
	// whose exceptions were all resolved drops to 0 instead of holding its
	// last count (a stale gauge is an alert that never clears).
	seenKinds map[string]bool
}

var _ ncd.Metrics = (*NCD)(nil)

// NewNCD registers the plane's metrics on reg.
func NewNCD(serviceName string, reg prometheus.Registerer) *NCD {
	labels := prometheus.Labels{"service": serviceName}
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: labels})
	}
	m := &NCD{
		AttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_ncd_attempts_total",
			Help:        "Provider submissions by channel, binding and normalized attempt state (ACCEPTED, DELIVERED, FAILED, UNKNOWN).",
			ConstLabels: labels,
		}, []string{"channel", "binding", "state"}),
		SubmitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "notification_ncd_submit_duration_seconds",
			Help:        "Time from submitting to a provider to its answer (§13.1 provider submit).",
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 30},
			ConstLabels: labels,
		}, []string{"channel"}),
		CallbacksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_ncd_callbacks_total",
			Help:        "Provider callbacks by binding and outcome: rejected_<code> at authentication, else each event's result (§13.1 callback integrity).",
			ConstLabels: labels,
		}, []string{"binding", "outcome"}),
		UnknownAttempts:           gauge("notification_ncd_unknown_attempts", "Attempts whose outcome is UNKNOWN pending reconciliation (§13.1 unknown resolution)."),
		UnknownOldestAgeSeconds:   gauge("notification_ncd_unknown_oldest_age_seconds", "Age of the oldest UNKNOWN attempt. The alertable one."),
		QueuedJobs:                gauge("notification_ncd_queued_jobs", "Delivery jobs queued for submission."),
		QueuedOldestAgeSeconds:    gauge("notification_ncd_queued_oldest_age_seconds", "How long the oldest due job has waited past its run time (§6.5 backpressure)."),
		NoticesPastDeadline:       gauge("notification_ncd_notices_past_deadline", "Regulated notices at their deadline without a concluded disposition (§13.2 deadline risk)."),
		RecordDeclarationsPending: gauge("notification_ncd_record_declarations_pending", "Regulated notices whose DRC record declaration is still pending (§13.1 record handoff)."),
		RecordPendingOldestAge:    gauge("notification_ncd_record_pending_oldest_age_seconds", "Age of the oldest pending DRC record declaration."),
		OpenExceptions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "notification_ncd_open_exceptions",
			Help:        "Unresolved exceptions by kind (§13.1, §13.2): the plane's human work queue.",
			ConstLabels: labels,
		}, []string{"kind"}),
		OpenExceptionOldestAge: gauge("notification_ncd_open_exception_oldest_age_seconds", "Age of the oldest unresolved exception of any kind."),
		seenKinds:              map[string]bool{},
	}
	reg.MustRegister(m.AttemptsTotal, m.SubmitDuration, m.CallbacksTotal, m.UnknownAttempts, m.UnknownOldestAgeSeconds,
		m.QueuedJobs, m.QueuedOldestAgeSeconds, m.NoticesPastDeadline, m.RecordDeclarationsPending, m.RecordPendingOldestAge,
		m.OpenExceptions, m.OpenExceptionOldestAge)
	return m
}

// AttemptSubmitted implements ncd.Metrics.
func (m *NCD) AttemptSubmitted(channel, binding, state string, took time.Duration) {
	m.AttemptsTotal.WithLabelValues(channel, binding, state).Inc()
	m.SubmitDuration.WithLabelValues(channel).Observe(took.Seconds())
}

// Callback implements ncd.Metrics.
func (m *NCD) Callback(binding, outcome string) {
	m.CallbacksTotal.WithLabelValues(binding, outcome).Inc()
}

// Backlog implements ncd.Metrics.
func (m *NCD) Backlog(b ncd.Backlog) {
	m.UnknownAttempts.Set(float64(b.UnknownAttempts))
	m.UnknownOldestAgeSeconds.Set(b.OldestUnknown.Seconds())
	m.QueuedJobs.Set(float64(b.QueuedJobs))
	m.QueuedOldestAgeSeconds.Set(b.OldestQueued.Seconds())
	m.NoticesPastDeadline.Set(float64(b.NoticesPastDeadline))
	m.RecordDeclarationsPending.Set(float64(b.RecordDeclarationsPending))
	m.RecordPendingOldestAge.Set(b.OldestRecordPending.Seconds())
	for kind := range b.OpenExceptions {
		m.seenKinds[kind] = true
	}
	for kind := range m.seenKinds {
		m.OpenExceptions.WithLabelValues(kind).Set(float64(b.OpenExceptions[kind]))
	}
	m.OpenExceptionOldestAge.Set(b.OldestOpenException.Seconds())
}

// RegisterSenderAuth exports the NP-55 sender-authentication state as
// notification_sender_auth_healthy (1 healthy, 0 email held) and returns its
// setter. Call it only when this service signs (DKIM configured): where the
// provider signs there is no monitor, and an always-0 gauge would page for a
// hold that is not happening.
func RegisterSenderAuth(serviceName string, reg prometheus.Registerer) func(healthy bool) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "notification_sender_auth_healthy",
		Help:        "1 while DKIM/DMARC/SPF authenticate this service's mail; 0 while email is held for a broken sender domain (§11.1, NP-55).",
		ConstLabels: prometheus.Labels{"service": serviceName},
	})
	reg.MustRegister(g)
	return func(healthy bool) {
		if healthy {
			g.Set(1)
		} else {
			g.Set(0)
		}
	}
}
