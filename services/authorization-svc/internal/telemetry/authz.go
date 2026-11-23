package telemetry

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// AuthzMetrics is the ZS-IAM-001 §26 signal set: decision rates by outcome and
// deciding layer, PDP latency, cache health, outbox health, privileged-session
// counts and projection lag (the PDP half of revocation lag).
//
// None of it is authority (§26 "never use metrics as authority"): every number
// here is read from what the service already decided or stored.
//
// Families (all carry const label service="authorization-svc"):
//
//	authorization_decisions_total{decision,outcome,layer}
//	authorization_decisions_by_action_total{action,decision}
//	authorization_evaluation_seconds
//	authorization_decision_record_failures_total
//	authorization_cache_hits_total / _misses_total / _invalidations_total
//	authorization_cache_entries
//	authorization_watermark_age_seconds
//	authorization_outbox_pending / _oldest_pending_seconds / _stuck
//	authorization_sessions{kind,state}
//	authorization_sessions_expiring_soon{kind}
//	authorization_projection_events_total{consumer,event_type}
//	authorization_projection_lag_seconds{consumer}
//	authorization_sampler_errors_total
type AuthzMetrics struct {
	decisions        *prometheus.CounterVec
	decisionsAction  *prometheus.CounterVec
	evaluation       prometheus.Histogram
	recordFailures   prometheus.Counter
	cacheHits        prometheus.CounterFunc
	cacheMisses      prometheus.CounterFunc
	cacheInvalidated prometheus.CounterFunc
	cacheEntries     prometheus.GaugeFunc
	watermarkAge     prometheus.Gauge
	outboxPending    prometheus.Gauge
	outboxOldest     prometheus.Gauge
	outboxStuck      prometheus.Gauge
	sessions         *prometheus.GaugeVec
	sessionsExpiring *prometheus.GaugeVec
	projectionEvents *prometheus.CounterVec
	projectionLag    *prometheus.HistogramVec
	samplerErrors    prometheus.Counter
}

// CacheStats is what the cache exposes to the scrape: cumulative counters and
// the age of the version watermark (zero ok = no watermark source).
type CacheStats func() (hits, misses, invalidations uint64, entries int, watermarkAge time.Duration, watermarkOK bool)

// OperationalSnapshot is one sampler read of the store.
type OperationalSnapshot struct {
	OutboxPending              int64
	OutboxOldestPendingSeconds float64
	OutboxStuck                int64
	// Sessions[kind][state] for kind jit|break_glass|support and state
	// ACTIVE|PENDING_APPROVAL.
	Sessions map[string]map[string]int64
	// ExpiringSoon[kind]: ACTIVE sessions ending within 15 minutes.
	ExpiringSoon map[string]int64
}

// NewAuthzMetrics registers the families on reg (prometheus.DefaultRegisterer
// in main). cache may be nil (caching disabled): the cache families then read 0.
func NewAuthzMetrics(reg prometheus.Registerer, serviceName string, cache CacheStats) *AuthzMetrics {
	cl := prometheus.Labels{"service": serviceName}
	stats := func() (uint64, uint64, uint64, int, time.Duration, bool) {
		if cache == nil {
			return 0, 0, 0, 0, 0, false
		}
		return cache()
	}
	m := &AuthzMetrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authorization_decisions_total", ConstLabels: cl,
			Help: "Recorded authorization decisions by canonical decision, /v1/authorize outcome and the layer that decided.",
		}, []string{"decision", "outcome", "layer"}),
		decisionsAction: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authorization_decisions_by_action_total", ConstLabels: cl,
			Help: "Recorded authorization decisions by protected action and canonical decision.",
		}, []string{"action", "decision"}),
		evaluation: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "authorization_evaluation_seconds", ConstLabels: cl,
			Help:    "Time the decision pipeline took to evaluate one request (PDP latency, excluding the decision write).",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		recordFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "authorization_decision_record_failures_total", ConstLabels: cl,
			Help: "Decisions refused with 503 because the decision artefact could not be written.",
		}),
		cacheHits: prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "authorization_cache_hits_total", ConstLabels: cl, Help: "Evaluation reads answered from the cache.",
		}, func() float64 { h, _, _, _, _, _ := stats(); return float64(h) }),
		cacheMisses: prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "authorization_cache_misses_total", ConstLabels: cl, Help: "Evaluation reads that went to the database.",
		}, func() float64 { _, mi, _, _, _, _ := stats(); return float64(mi) }),
		cacheInvalidated: prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "authorization_cache_invalidations_total", ConstLabels: cl, Help: "Cache namespace invalidations (writes, events, commands).",
		}, func() float64 { _, _, inv, _, _, _ := stats(); return float64(inv) }),
		cacheEntries: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "authorization_cache_entries", ConstLabels: cl, Help: "Entries currently held by the evaluation cache.",
		}, func() float64 { _, _, _, e, _, _ := stats(); return float64(e) }),
		watermarkAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "authorization_watermark_age_seconds", ConstLabels: cl,
			Help: "Seconds since the version watermark was last read (-1 when there is no watermark source). Past 3 s the cache is bypassed.",
		}),
		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "authorization_outbox_pending", ConstLabels: cl, Help: "Outbox events not yet published.",
		}),
		outboxOldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "authorization_outbox_oldest_pending_seconds", ConstLabels: cl, Help: "Age of the oldest unpublished outbox event.",
		}),
		outboxStuck: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "authorization_outbox_stuck", ConstLabels: cl,
			Help: "Unpublished outbox events that have failed 5 or more publish attempts (the relay keeps retrying; there is no dead-letter).",
		}),
		sessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "authorization_sessions", ConstLabels: cl, Help: "Privileged sessions by kind and state.",
		}, []string{"kind", "state"}),
		sessionsExpiring: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "authorization_sessions_expiring_soon", ConstLabels: cl, Help: "ACTIVE privileged sessions ending within 15 minutes.",
		}, []string{"kind"}),
		projectionEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authorization_projection_events_total", ConstLabels: cl, Help: "Consumed events applied to this service's projections.",
		}, []string{"consumer", "event_type"}),
		projectionLag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "authorization_projection_lag_seconds", ConstLabels: cl,
			Help:    "Time from an upstream event's emitted_at to its projection here: the PDP half of revocation lag.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300},
		}, []string{"consumer"}),
		samplerErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "authorization_sampler_errors_total", ConstLabels: cl, Help: "Operational sampler reads that failed.",
		}),
	}
	m.watermarkAge.Set(-1)
	// The two consumers exist from boot, so their lag series do too (count 0)
	// rather than appearing only after the first event — an alert on a
	// missing series cannot tell "no events" from "not wired".
	for _, consumer := range []string{"delegation", "lifecycle"} {
		m.projectionLag.WithLabelValues(consumer)
	}
	reg.MustRegister(m.decisions, m.decisionsAction, m.evaluation, m.recordFailures,
		m.cacheHits, m.cacheMisses, m.cacheInvalidated, m.cacheEntries, m.watermarkAge,
		m.outboxPending, m.outboxOldest, m.outboxStuck, m.sessions, m.sessionsExpiring,
		m.projectionEvents, m.projectionLag, m.samplerErrors)
	return m
}

// RegisterSIEMDrops exposes the SIEM client's drop counter as
// authorization_siem_dropped_total: security events discarded because the
// streaming queue was full (siem-integration-svc slow or down).
func (m *AuthzMetrics) RegisterSIEMDrops(reg prometheus.Registerer, serviceName string, dropped func() uint64) {
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "authorization_siem_dropped_total", ConstLabels: prometheus.Labels{"service": serviceName},
		Help: "Security events not streamed to siem-integration-svc because the bounded queue was full.",
	}, func() float64 { return float64(dropped()) }))
}

// ObserveDecision counts one recorded decision.
func (m *AuthzMetrics) ObserveDecision(decision, outcome, layer, action string) {
	m.decisions.WithLabelValues(decision, outcome, layer).Inc()
	m.decisionsAction.WithLabelValues(action, decision).Inc()
}

// ObserveEvaluation records the pipeline's own time for one request.
func (m *AuthzMetrics) ObserveEvaluation(d time.Duration) { m.evaluation.Observe(d.Seconds()) }

// DecisionRecordFailed counts a decision refused because its artefact could not
// be written.
func (m *AuthzMetrics) DecisionRecordFailed() { m.recordFailures.Inc() }

// ObserveProjection counts one applied consumed event and its lag.
func (m *AuthzMetrics) ObserveProjection(consumer, eventType string, lag time.Duration, hasLag bool) {
	m.projectionEvents.WithLabelValues(consumer, eventType).Inc()
	if hasLag && lag >= 0 {
		m.projectionLag.WithLabelValues(consumer).Observe(lag.Seconds())
	}
}

// Apply writes one snapshot into the gauges. Exported for tests.
func (m *AuthzMetrics) Apply(s OperationalSnapshot) {
	m.outboxPending.Set(float64(s.OutboxPending))
	m.outboxOldest.Set(s.OutboxOldestPendingSeconds)
	m.outboxStuck.Set(float64(s.OutboxStuck))
	for _, kind := range []string{"jit", "break_glass", "support"} {
		for _, state := range []string{"ACTIVE", "PENDING_APPROVAL"} {
			m.sessions.WithLabelValues(kind, state).Set(float64(s.Sessions[kind][state]))
		}
		m.sessionsExpiring.WithLabelValues(kind).Set(float64(s.ExpiringSoon[kind]))
	}
}

// RunSampler reads the store every interval (and the watermark age every
// second) until ctx ends. A failed read keeps the previous values and counts
// an error; it never stops the loop.
func (m *AuthzMetrics) RunSampler(ctx context.Context, read func(context.Context) (OperationalSnapshot, error), cache CacheStats, every time.Duration, log *zap.Logger) {
	sample := func() {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := read(rctx)
		if err != nil {
			m.samplerErrors.Inc()
			if ctx.Err() == nil && !strings.Contains(err.Error(), "context canceled") {
				log.Warn("operational metrics sample failed", zap.Error(err))
			}
			return
		}
		m.Apply(s)
	}
	watermark := func() {
		if cache == nil {
			return
		}
		if _, _, _, _, age, ok := cache(); ok {
			m.watermarkAge.Set(age.Seconds())
		}
	}
	if every <= 0 {
		every = 15 * time.Second
	}
	sample()
	watermark()
	slow := time.NewTicker(every)
	fast := time.NewTicker(time.Second)
	defer slow.Stop()
	defer fast.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			sample()
		case <-fast.C:
			watermark()
		}
	}
}
