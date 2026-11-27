package telemetry

import (
	"context"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// CertWatchMetrics are the certification watch's operational metrics (ZS-SVC-Z-001 RB-05, DQC-017). It satisfies certwatch.Observer.
//
// Each exists to back an alert in deployments/prometheus-rules.yml (group gl-certification-watch). Labels are closed sets (outcome,
// result, trigger, kind): nothing carries a tenant, an entity, a certification or a journal, so cardinality cannot grow with data.
type CertWatchMetrics struct {
	interval    prometheus.Gauge
	runs        *prometheus.CounterVec
	lastRun     prometheus.Gauge
	lastSuccess prometheus.Gauge
	duration    prometheus.Histogram
	scopes      *prometheus.CounterVec
	unchecked   prometheus.Counter
	flags       *prometheus.CounterVec
	events      *prometheus.CounterVec
	reactions   *prometheus.CounterVec
	consumerErr *prometheus.CounterVec
	now         func() time.Time
}

var _ certwatch.Observer = (*CertWatchMetrics)(nil)

// NewCertWatchMetrics registers the metrics on the default registry.
func NewCertWatchMetrics(serviceName string) *CertWatchMetrics {
	labels := prometheus.Labels{"service": serviceName}
	m := &CertWatchMetrics{now: time.Now}
	m.interval = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gl_cert_sweep_interval_seconds", ConstLabels: labels,
		Help: "The configured interval of the scheduled certification sweep in seconds; 0 when the schedule is off (no principal, or CERT_SWEEP_INTERVAL=off)."})
	m.runs = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_sweep_runs_total", ConstLabels: labels,
		Help: "Scheduled sweep passes by outcome: ok, partial (a scope skipped or a certification unchecked), error (the pass could not run) or locked (another replica holds it)."}, []string{"outcome"})
	m.lastRun = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gl_cert_sweep_last_run_timestamp_seconds", ConstLabels: labels,
		Help: "Unix time of this replica's last completed sweep pass, whatever its outcome."})
	m.lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gl_cert_sweep_last_success_timestamp_seconds", ConstLabels: labels,
		Help: "Unix time of this replica's last sweep pass that skipped nothing and left nothing unchecked. Alert on the MAX across replicas: only the replica holding the lock sweeps."})
	m.duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "gl_cert_sweep_duration_seconds", ConstLabels: labels,
		Help: "Duration of a scheduled sweep pass that ran (not one that found the lock held).", Buckets: []float64{.05, .25, 1, 2.5, 5, 15, 30, 60, 120, 300, 600}})
	m.scopes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_sweep_scopes_total", ConstLabels: labels,
		Help: "Scopes (a tenant and legal entity) of scheduled passes by result: swept, unauthorized (the sweep principal is not granted the sweep permission) or error."}, []string{"result"})
	m.unchecked = prometheus.NewCounter(prometheus.CounterOpts{Name: "gl_cert_sweep_certifications_unchecked_total", ConstLabels: labels,
		Help: "Certifications a scheduled pass could not check (the issuing service unreachable, or an answer it did not recognise). They are never treated as in force."})
	m.flags = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_flags_raised_total", ConstLabels: labels,
		Help: "Flags raised on journals and postings resting on a revoked certification, by the route that found them: schedule, event or manual."}, []string{"trigger"})
	m.events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_withdrawal_events_total", ConstLabels: labels,
		Help: "Reliance-withdrawn deliveries by result: handled, irrelevant, malformed (this service's but unreadable) or failed (after the retries; the schedule covers it)."}, []string{"result"})
	m.reactions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_withdrawal_reactions_total", ConstLabels: labels,
		Help: "A withdrawal event's response per legal entity: swept, unauthorized, unwatched (the certification is not on the watch list) or failed."}, []string{"result"})
	m.consumerErr = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gl_cert_consumer_errors_total", ConstLabels: labels,
		Help: "Broker problems of the withdrawal event consumer by kind: read or commit."}, []string{"kind"})
	prometheus.MustRegister(m.interval, m.runs, m.lastRun, m.lastSuccess, m.duration, m.scopes, m.unchecked, m.flags, m.events, m.reactions, m.consumerErr)
	return m
}

// SweepScheduled implements certwatch.Observer.
func (m *CertWatchMetrics) SweepScheduled(interval time.Duration) { m.interval.Set(interval.Seconds()) }

// SweepRun implements certwatch.Observer.
func (m *CertWatchMetrics) SweepRun(outcome string, elapsed time.Duration) {
	m.runs.WithLabelValues(outcome).Inc()
	if outcome == certwatch.RunLocked || outcome == certwatch.RunError {
		return // neither swept anything: not a completed pass, and not a success
	}
	now := float64(m.now().Unix())
	m.lastRun.Set(now)
	m.duration.Observe(elapsed.Seconds())
	if outcome == certwatch.RunOK {
		m.lastSuccess.Set(now)
	}
}

// SweepScope implements certwatch.Observer.
func (m *CertWatchMetrics) SweepScope(result string) { m.scopes.WithLabelValues(result).Inc() }

// CertificationsUnchecked implements certwatch.Observer.
func (m *CertWatchMetrics) CertificationsUnchecked(n int) {
	if n > 0 {
		m.unchecked.Add(float64(n))
	}
}

// FlagsRaised implements certwatch.Observer.
func (m *CertWatchMetrics) FlagsRaised(trigger string, n int) {
	if n > 0 {
		m.flags.WithLabelValues(trigger).Add(float64(n))
	}
}

// WithdrawalEvent implements certwatch.Observer.
func (m *CertWatchMetrics) WithdrawalEvent(result string) { m.events.WithLabelValues(result).Inc() }

// WithdrawalReaction implements certwatch.Observer.
func (m *CertWatchMetrics) WithdrawalReaction(result string) {
	m.reactions.WithLabelValues(result).Inc()
}

// ConsumerError implements certwatch.Observer.
func (m *CertWatchMetrics) ConsumerError(kind string) { m.consumerErr.WithLabelValues(kind).Inc() }

// FlagStats reads the open-flag signals across tenants (a capability-scoped read, migration 000038).
type FlagStats func(ctx context.Context) (certwatch.OpenFlagStats, error)

// RegisterFlagGauges registers the two gauges read at scrape time, like the outbox backlog: the number of OPEN flags (journals and
// postings resting on a revoked certification that no person has decided on yet) and the age of the oldest. A read that fails
// reports NaN, never a reassuring zero.
func RegisterFlagGauges(serviceName string, read FlagStats, onError func(error)) {
	labels := prometheus.Labels{"service": serviceName}
	pick := func(f func(certwatch.OpenFlagStats) float64) func() float64 {
		return func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			st, err := read(ctx)
			if err != nil {
				if onError != nil {
					onError(err)
				}
				return math.NaN()
			}
			return f(st)
		}
	}
	prometheus.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "gl_cert_flags_open", ConstLabels: labels,
			Help: "Flags awaiting a person's decision: journals and postings that rest on a revoked certification."},
			pick(func(s certwatch.OpenFlagStats) float64 { return float64(s.Open) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "gl_cert_flags_oldest_open_age_seconds", ConstLabels: labels,
			Help: "Age in seconds of the oldest flag still awaiting a decision; 0 when none is open."},
			pick(func(s certwatch.OpenFlagStats) float64 { return s.Oldest.Seconds() })),
	)
}
