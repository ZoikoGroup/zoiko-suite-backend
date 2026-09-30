package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Domain holds the metrics that describe what this service DOES, as opposed to
// the HTTP metrics in telemetry.go, which describe how it is being called.
//
// The distinction is load-bearing, because every interesting failure in this
// service answers 2xx:
//
//   - A delivery that FAILED comes back 201, deliberately — §9.7 requires that
//     notification failure must not collapse the source workflow.
//   - A delivery rescheduled after a transient failure also comes back 201.
//   - A stranded notification — accepted, then never attempted again — has no
//     request associated with it at all.
//   - An event that never reached the bus used to be a log line and nothing
//     more.
//
// So http_requests_total{status_code="2xx"} is flat and healthy across every
// one of those. A dashboard built on the HTTP metrics alone cannot distinguish
// a service delivering every notice from one delivering none of them.
//
// Two series are deliberately NOT here: the per-provider attempt count and
// duration live on Metrics (notification_delivery_attempts_total and
// notification_delivery_duration_seconds, labelled stream/provider/status) and
// are recorded inside internal/deliver. The names below were chosen so that
// the two sets can be registered side by side — reusing those names with a
// different label set makes prometheus.MustRegister panic at startup.
type Domain struct {
	// DeliveriesTotal counts concluded deliveries by channel and outcome
	// (SENT / FAILED / PENDING_UNKNOWN). The ratio is the service's health.
	DeliveriesTotal *prometheus.CounterVec

	// AttemptsByOriginTotal counts delivery ATTEMPTS by where they were made.
	// "origin" separates the synchronous attempt inside the send request from
	// one made later by the retry worker: folding them together would hide a
	// service whose first attempt never works.
	AttemptsByOriginTotal *prometheus.CounterVec

	// AttemptDuration measures one attempt by channel. IN_APP is a database
	// write and EMAIL an SMTP session, so they are separated rather than
	// averaged into a number describing neither.
	AttemptDuration *prometheus.HistogramVec

	// RetriesScheduledTotal counts transient failures rescheduled rather than
	// concluded. A rising rate with a flat FAILED count is a relay that is slow
	// rather than broken.
	RetriesScheduledTotal *prometheus.CounterVec

	// RetriesExhaustedTotal counts notifications that ran out of attempts —
	// separate from FAILED because a rejected mailbox is a data problem and an
	// exhausted budget an infrastructure one.
	RetriesExhaustedTotal prometheus.Counter

	// StrandedReclaimedTotal counts notifications the sweep found in flight and
	// put back on the schedule. Each is a notice the platform accepted and then
	// lost track of, so any sustained non-zero rate is worth a human.
	StrandedReclaimedTotal prometheus.Counter

	// ── Outbox ──────────────────────────────────────────────────────────────
	//
	// OutboxPending and OutboxOldestAgeSeconds are the pair that makes a
	// stalled relay visible. Depth alone cannot: a backlog of ten that is three
	// seconds old is a busy service; one that is an hour old is an hour of
	// governed notices no consumer has been told about.
	OutboxPending          prometheus.Gauge
	OutboxOldestAgeSeconds prometheus.Gauge
	OutboxPublished        *prometheus.CounterVec
	OutboxFailures         prometheus.Counter
}

// NewDomain registers the domain metrics on the default registry. Called once
// from main; a duplicate registration is a programming error and panics.
func NewDomain(serviceName string) *Domain {
	return NewDomainWithRegisterer(serviceName, prometheus.DefaultRegisterer)
}

// NewDomainWithRegisterer registers on reg, so a test can use a fresh registry.
func NewDomainWithRegisterer(serviceName string, reg prometheus.Registerer) *Domain {
	labels := prometheus.Labels{"service": serviceName}

	d := &Domain{
		DeliveriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_deliveries_total",
			Help:        "Concluded notification deliveries by channel and resulting status.",
			ConstLabels: labels,
		}, []string{"channel", "status"}),

		AttemptsByOriginTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_attempts_by_origin_total",
			Help:        "Delivery attempts made, by channel, outcome and where the attempt originated (request or retry).",
			ConstLabels: labels,
		}, []string{"channel", "outcome", "origin"}),

		AttemptDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notification_attempt_duration_seconds",
			Help: "Duration of one delivery attempt, by channel.",
			// Not DefBuckets: IN_APP is a millisecond database write and an SMTP
			// session runs to seconds; DefBuckets stops at 10s, exactly the SMTP
			// timeout, so every timed-out attempt would land in +Inf.
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 30},
			ConstLabels: labels,
		}, []string{"channel"}),

		RetriesScheduledTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_retries_scheduled_total",
			Help:        "Transient delivery failures rescheduled for another attempt, by channel.",
			ConstLabels: labels,
		}, []string{"channel"}),

		RetriesExhaustedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "notification_retries_exhausted_total",
			Help:        "Notifications that failed terminally after using the whole retry budget.",
			ConstLabels: labels,
		}),

		StrandedReclaimedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "notification_stranded_reclaimed_total",
			Help:        "In-flight notifications the sweep found abandoned and put back on the retry schedule.",
			ConstLabels: labels,
		}),

		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "notification_outbox_pending",
			Help:        "Events committed with their state change but not yet published to Kafka.",
			ConstLabels: labels,
		}),

		OutboxOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "notification_outbox_oldest_age_seconds",
			Help:        "Age of the oldest unpublished outbox event, in seconds. The alertable one.",
			ConstLabels: labels,
		}),

		OutboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_outbox_published_total",
			Help:        "Outbox events successfully written to Kafka, by event type.",
			ConstLabels: labels,
		}, []string{"event_type"}),

		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "notification_outbox_publish_failures_total",
			Help:        "Outbox drains that could not reach Kafka. The events are retained and retried.",
			ConstLabels: labels,
		}),
	}

	reg.MustRegister(
		d.DeliveriesTotal,
		d.AttemptsByOriginTotal,
		d.AttemptDuration,
		d.RetriesScheduledTotal,
		d.RetriesExhaustedTotal,
		d.StrandedReclaimedTotal,
		d.OutboxPending,
		d.OutboxOldestAgeSeconds,
		d.OutboxPublished,
		d.OutboxFailures,
	)
	return d
}

// Origins for AttemptsByOriginTotal.
const (
	// OriginRequest is the synchronous attempt made inside POST /v1/notifications.
	OriginRequest = "request"
	// OriginRetry is an attempt made later by the retry worker.
	OriginRetry = "retry"
	// OriginResend is an explicit, reasoned resend (POST /{id}/resend).
	OriginResend = "resend"
)

// Outcomes for ObserveAttempt. One set of values shared by the handler and the
// worker: "sent" from one and "SENT" from the other would produce two series
// that never sum, and Prometheus reports no error for that.
const (
	OutcomeDelivered = "delivered"
	OutcomeFailed    = "failed"
	OutcomeRetrying  = "retrying"
	OutcomeUnknown   = "unknown"
)

// ObserveAttempt records one delivery attempt. Nil-safe, so a handler or
// worker built without metrics (tests) needs no guard at each call site.
func (d *Domain) ObserveAttempt(channel, outcome, origin string, seconds float64) {
	if d == nil {
		return
	}
	d.AttemptsByOriginTotal.WithLabelValues(channel, outcome, origin).Inc()
	d.AttemptDuration.WithLabelValues(channel).Observe(seconds)
}

// ObserveConclusion records a delivery reaching SENT, FAILED or PENDING_UNKNOWN.
func (d *Domain) ObserveConclusion(channel, status string) {
	if d == nil {
		return
	}
	d.DeliveriesTotal.WithLabelValues(channel, status).Inc()
}

// ObserveRetryScheduled records a transient failure being rescheduled.
func (d *Domain) ObserveRetryScheduled(channel string) {
	if d == nil {
		return
	}
	d.RetriesScheduledTotal.WithLabelValues(channel).Inc()
}

// ObserveRetryExhausted records a notification that used its whole budget.
func (d *Domain) ObserveRetryExhausted() {
	if d == nil {
		return
	}
	d.RetriesExhaustedTotal.Inc()
}

// ObserveStrandedReclaimed records the sweep putting a stranded row back.
func (d *Domain) ObserveStrandedReclaimed() {
	if d == nil {
		return
	}
	d.StrandedReclaimedTotal.Inc()
}
