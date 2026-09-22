package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Domain holds the metrics that describe what this service DOES, as opposed to
// the HTTP metrics in telemetry.go, which describe how it is being called.
//
// The distinction is load-bearing here, and it is why this file exists. Every
// interesting failure in this service answers 2xx:
//
//   - A delivery that FAILED comes back 201, deliberately — §9.7 requires that
//     notification failure must not collapse the source workflow, so a payroll
//     run that finalized correctly is not told it failed because an employee
//     has no address on file.
//   - A delivery rescheduled after a transient failure also comes back 201.
//   - A stranded notification — accepted, then never attempted again — has no
//     request associated with it at all; the request that created it succeeded
//     days earlier.
//   - An event that never reached the bus used to be a log line and nothing
//     more.
//
// So http_requests_total{status_code="2xx"} is flat and healthy across every
// one of those, and before this file there was no number anywhere that moved.
// A dashboard built on the HTTP metrics alone cannot distinguish a service
// delivering every notice from one delivering none of them.
type Domain struct {
	// DeliveriesTotal counts concluded deliveries by channel and outcome
	// (SENT / FAILED). The ratio is the service's actual health.
	DeliveriesTotal *prometheus.CounterVec

	// AttemptsTotal counts delivery ATTEMPTS, which is not the same thing:
	// one notification delivered on its fourth try is one delivery and four
	// attempts, and the gap between the two counters is how much the mail
	// relay is costing.
	AttemptsTotal *prometheus.CounterVec

	// RetriesScheduledTotal counts transient failures that were rescheduled
	// rather than concluded. A rising rate with a flat FAILED count is a relay
	// that is slow rather than broken.
	RetriesScheduledTotal *prometheus.CounterVec

	// RetriesExhaustedTotal counts notifications that ran out of attempts.
	// Separated from the ordinary FAILED count because the two have different
	// causes and different fixes: a rejected mailbox is a data problem, an
	// exhausted budget is an infrastructure one.
	RetriesExhaustedTotal prometheus.Counter

	// StrandedReclaimedTotal counts notifications the sweep found in flight
	// and put back on the schedule. Every one of them is a notice the platform
	// accepted and then lost track of, so this should normally be zero and any
	// sustained non-zero rate is worth a human.
	StrandedReclaimedTotal prometheus.Counter

	// DeliveryDuration measures one attempt against a provider, by channel.
	// IN_APP is a database write and EMAIL is an SMTP session, so they are
	// separated rather than averaged into a number describing neither.
	DeliveryDuration *prometheus.HistogramVec

	// ── Outbox ──────────────────────────────────────────────────────────────
	//
	// OutboxPending and OutboxOldestAgeSeconds are the pair that makes a
	// stalled relay visible. Depth alone cannot: a backlog of ten that is
	// three seconds old is a busy service, and a backlog of ten that is an
	// hour old is an hour of governed notices whose issue no consumer has been
	// told about while the register shows every one of them concluded.
	OutboxPending          prometheus.Gauge
	OutboxOldestAgeSeconds prometheus.Gauge
	OutboxPublished        *prometheus.CounterVec
	OutboxFailures         prometheus.Counter
}

// NewDomain registers the domain metrics.
//
// prometheus.MustRegister, matching NewMetrics in telemetry.go: a duplicate
// registration is a programming error that must fail at startup, not a
// condition to handle. It is called once from main.
func NewDomain(serviceName string) *Domain {
	labels := prometheus.Labels{"service": serviceName}

	d := &Domain{
		DeliveriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_deliveries_total",
			Help:        "Concluded notification deliveries by channel and final status.",
			ConstLabels: labels,
		}, []string{"channel", "status"}),

		AttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notification_delivery_attempts_total",
			// "origin" separates the synchronous attempt made inside the send
			// request from one made later by the retry worker. Folding them
			// together would hide a service whose first attempt never works.
			Help:        "Delivery attempts made, by channel, outcome and where the attempt originated.",
			ConstLabels: labels,
		}, []string{"channel", "outcome", "origin"}),

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

		DeliveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notification_delivery_duration_seconds",
			Help: "Duration of one delivery attempt against the provider, by channel.",
			// Not DefBuckets. An IN_APP delivery is a local database write in
			// single-digit milliseconds, and an SMTP session against a remote
			// relay runs to seconds — DefBuckets tops out at 10s, which is
			// exactly the SMTP timeout, so every timed-out attempt would land
			// in +Inf and be indistinguishable from any other slow one.
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 30},
			ConstLabels: labels,
		}, []string{"channel"}),

		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "notification_outbox_pending",
			Help:        "Events committed with a delivery conclusion but not yet published to Kafka.",
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

	prometheus.MustRegister(
		d.DeliveriesTotal,
		d.AttemptsTotal,
		d.RetriesScheduledTotal,
		d.RetriesExhaustedTotal,
		d.StrandedReclaimedTotal,
		d.DeliveryDuration,
		d.OutboxPending,
		d.OutboxOldestAgeSeconds,
		d.OutboxPublished,
		d.OutboxFailures,
	)
	return d
}

// Origins for AttemptsTotal.
const (
	// OriginRequest is the synchronous attempt made inside POST /v1/notifications.
	OriginRequest = "request"
	// OriginRetry is an attempt made later by the retry worker.
	OriginRetry = "retry"
)

// ObserveAttempt records one delivery attempt.
//
// A method rather than three call sites poking at the vectors, because the
// label values have to agree across the handler and the worker: "sent" from one
// and "SENT" from the other would produce two series that never sum, and
// Prometheus reports no error for that.
func (d *Domain) ObserveAttempt(channel, outcome, origin string, seconds float64) {
	if d == nil {
		return
	}
	d.AttemptsTotal.WithLabelValues(channel, outcome, origin).Inc()
	d.DeliveryDuration.WithLabelValues(channel).Observe(seconds)
}

// ObserveConclusion records a delivery reaching SENT or FAILED.
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

// Outcomes for ObserveAttempt.
const (
	OutcomeDelivered = "delivered"
	OutcomeFailed    = "failed"
)
