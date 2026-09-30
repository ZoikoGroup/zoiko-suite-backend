package telemetry

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// GOV-01 metrics.
//
// These are the numbers the golden-signals dashboard CANNOT infer from request
// rates, and each exists because something important about this service is
// otherwise invisible:
//
//   - the outbox depth, because a stopped relay produces no errors and no
//     latency — every request succeeds while governance events pile up in a
//     table nobody is watching;
//   - the security refusals, because an ingress/tenant mismatch is a 401 like
//     any other 401 in the HTTP metrics, and it is not like any other 401;
//   - the break-glass counters, because a live elevation into a customer
//     tenant is a fact the security team should be able to see without
//     querying this service's database;
//   - the disposition pair, because "disposed nothing" and "held everything"
//     are the same picture if you only plot one of them.

// GovMetrics holds the GOV-01-specific instrumentation.
type GovMetrics struct {
	OutboxPending    prometheus.Gauge
	OutboxDeadLetter prometheus.Gauge
	OutboxPublished  prometheus.Counter
	OutboxFailed     prometheus.Counter

	// ResolutionFailed is labelled by REASON, which is the whole point:
	// ingress_tenant_mismatch and token_invalid are both 401s in the HTTP
	// metrics and could not be less alike operationally.
	ResolutionFailed *prometheus.CounterVec

	RiskSignalUnavailable prometheus.Counter

	SupportContextsLive       prometheus.Gauge
	SupportContextsUnreviewed prometheus.Gauge
	SupportContextsGranted    *prometheus.CounterVec

	// SoDChecks is labelled by outcome. UNAVAILABLE is the one to watch: the
	// command was correctly refused, but a sustained rate means GOV-04 is down
	// and no privileged command can be issued at all.
	SoDChecks *prometheus.CounterVec

	DispositionDisposed prometheus.Counter
	DispositionHeld     prometheus.Counter
}

// NewGovMetrics registers the GOV-01 instruments.
func NewGovMetrics(serviceName string) *GovMetrics {
	labels := prometheus.Labels{"service": serviceName}

	m := &GovMetrics{
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "identity_context_outbox_pending",
			Help:        "Events written to the outbox and not yet delivered. THE number to alert on: a stopped relay is invisible from the request path.",
			ConstLabels: labels,
		}),
		OutboxDeadLetter: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "identity_context_outbox_dead_letter",
			Help:        "Events that exhausted their delivery attempts. Not deleted — they remain in event_outbox with last_error set. Any non-zero value needs a human.",
			ConstLabels: labels,
		}),
		OutboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "identity_context_outbox_published_total",
			Help:        "Events successfully delivered to the broker by the relay.",
			ConstLabels: labels,
		}),
		OutboxFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "identity_context_outbox_failed_total",
			Help:        "Relay delivery attempts that failed and were rescheduled.",
			ConstLabels: labels,
		}),

		ResolutionFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "identity_context_resolution_failed_total",
			Help:        "Refused resolutions by reason. ingress_tenant_mismatch and residency_denied are security events; the rest are ordinary.",
			ConstLabels: labels,
		}, []string{"reason"}),

		RiskSignalUnavailable: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "identity_context_risk_signal_unavailable_total",
			Help:        "Resolutions where no risk signal existed, so trust posture was DEFAULTED rather than measured.",
			ConstLabels: labels,
		}),

		SupportContextsLive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "identity_context_support_contexts_live",
			Help:        "Privileged elevations into customer tenants that are active right now. Zero in steady state.",
			ConstLabels: labels,
		}),
		SupportContextsUnreviewed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "identity_context_support_contexts_unreviewed",
			Help:        "Support contexts that ended and were never reconciled — the half of break-glass that is normally missing.",
			ConstLabels: labels,
		}),
		SupportContextsGranted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "identity_context_support_contexts_granted_total",
			Help:        "Support elevations granted, by reason code.",
			ConstLabels: labels,
		}, []string{"reason_code"}),

		SoDChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "identity_context_sod_checks_total",
			Help:        "Segregation-of-duties evaluations by outcome: no_conflict, conflict, unavailable.",
			ConstLabels: labels,
		}, []string{"outcome"}),

		DispositionDisposed: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "identity_context_disposition_disposed_total",
			Help:        "Session evidence rows disposed under retention.",
			ConstLabels: labels,
		}),
		DispositionHeld: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "identity_context_disposition_held_total",
			Help:        "Rows that were due for disposition but blocked by an active legal hold. Must be read alongside the disposed counter.",
			ConstLabels: labels,
		}),
	}

	prometheus.MustRegister(
		m.OutboxPending, m.OutboxDeadLetter, m.OutboxPublished, m.OutboxFailed,
		m.ResolutionFailed, m.RiskSignalUnavailable,
		m.SupportContextsLive, m.SupportContextsUnreviewed, m.SupportContextsGranted,
		m.SoDChecks,
		m.DispositionDisposed, m.DispositionHeld,
	)
	return m
}

// OutboxStatsSource is what the sampler reads.
type OutboxStatsSource interface {
	PendingCount(ctx context.Context) (int, error)
	DeadLetterCount(ctx context.Context) (int, error)
	Stats() (published, failed int64)
}

// SampleOutbox polls the outbox depth on an interval until ctx is cancelled.
//
// POLLED RATHER THAN INCREMENTED at the enqueue site, deliberately. The depth
// is a property of the TABLE, not of this process: several replicas share one
// outbox, and a counter maintained per process would report each replica's own
// contribution and miss a backlog left behind by a replica that has since been
// replaced — which is exactly the situation worth alerting on.
//
// The interval is generous. This is a COUNT(*) on an indexed partial predicate
// and it is cheap, but it is not free, and a backlog that matters will still
// be there in thirty seconds.
func SampleOutbox(ctx context.Context, src OutboxStatsSource, m *GovMetrics, interval time.Duration, log *zap.Logger) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastPublished, lastFailed int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

			if pending, err := src.PendingCount(sampleCtx); err == nil {
				m.OutboxPending.Set(float64(pending))
			} else {
				log.Debug("outbox pending sample failed", zap.Error(err))
			}

			if dead, err := src.DeadLetterCount(sampleCtx); err == nil {
				m.OutboxDeadLetter.Set(float64(dead))
			}

			// The relay keeps running totals; Prometheus counters only go up,
			// so the DELTA is added rather than the total set. Setting would
			// break rate() across a restart in the other direction.
			published, failed := src.Stats()
			if d := published - lastPublished; d > 0 {
				m.OutboxPublished.Add(float64(d))
			}
			if d := failed - lastFailed; d > 0 {
				m.OutboxFailed.Add(float64(d))
			}
			lastPublished, lastFailed = published, failed

			cancel()
		}
	}
}
