// Package metrics exposes the assistant's Prometheus metrics (Doc 08 §14,
// "Metric" row): request counts with outcome labels, latency histograms for
// p50/p95/p99, dead-lettered events and model cost units (tokens).
//
// Labels are low-cardinality enums only: route pattern, method, status code,
// turn status and reason, model result, limit scope. Session ids, request
// ids, intents with free text, markets and message content are never labels
// (Doc 08: "Session or request IDs as metric labels" are excluded).
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds only this service's metrics (no global default registry).
var Registry = prometheus.NewRegistry()

var (
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_http_requests_total",
		Help: "HTTP requests by route pattern, method and status code.",
	}, []string{"route", "method", "code"})

	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "public_ai_http_request_duration_seconds",
		Help:    "HTTP request latency by route pattern (gate target p95 <= 0.5 s, Doc 04).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 8, 10, 15, 30},
	}, []string{"route"})

	turns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_turns_total",
		Help: "Completed turns by envelope status and reason.",
	}, []string{"status", "reason"})

	turnDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "public_ai_turn_duration_seconds",
		Help:    "Turn latency by envelope status (answer target p95 <= 8 s, Doc 04).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 6, 8, 10, 15, 30},
	}, []string{"status"})

	modelRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_model_requests_total",
		Help: "Model gateway calls by result: ok, timeout, transient, provider, circuit_open.",
	}, []string{"result"})

	modelTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_model_tokens_total",
		Help: "Model cost units: tokens by direction (input, output).",
	}, []string{"direction"})

	rateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_rate_limited_total",
		Help: "Requests refused with 429 by scope: ip, session, inflight.",
	}, []string{"scope"})

	eventsPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "public_ai_events_published_total",
		Help: "Outbox events delivered to the broker.",
	})

	eventsDeadLettered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "public_ai_events_dead_lettered_total",
		Help: "Outbox events diverted to a .dlq topic after repeated failures.",
	})

	// Doc 09 §20 / Doc 10 M01-M02: offers and clicks are separate measures.
	ctaOffered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_cta_offered_total",
		Help: "Official-route CTAs offered in answers, by route_key.",
	}, []string{"route_key"})

	handoffClicked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_handoff_clicked_total",
		Help: "Official routes opened by visitors, by route_key (never a case, lead or booking).",
	}, []string{"route_key"})

	feedback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_feedback_total",
		Help: "Answer feedback by rating and reason enum (no free text).",
	}, []string{"rating", "reason"})

	securitySignals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_security_signals_total",
		Help: "Security signals (prompt injection etc.) by reason class.",
	}, []string{"reason_class"})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "public_ai_build_info",
		Help: "Always 1: the app version and model profile of this process (Doc 08 §06 release tuple).",
	}, []string{"git_sha", "model_profile"})

	spendRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "public_ai_spend_ratio",
		Help: "Model token usage as a fraction of the daily or monthly ceiling (alerts at 0.5, 0.8, 1.0; Doc 04 §25).",
	}, []string{"window"})

	stageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "public_ai_stage_duration_seconds",
		Help:    "Time spent per pipeline stage: retrieve, model, postgate (Doc 04 §23).",
		Buckets: []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"stage"})

	turnOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "public_ai_turn_outcomes_total",
		Help: "Turn outcomes by status, knowledge release and market (\"none\" or an offered market; Doc 05 §25).",
	}, []string{"status", "release", "market"})

	outboxOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "public_ai_outbox_oldest_pending_seconds",
		Help: "Age of the oldest event in the last relay batch (outbox lag, Doc 05 §25).",
	})

	commitRevocations = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "public_ai_commit_revocation_rejections_total",
		Help: "Answers withheld at commit because a cited source or claim was revoked while the turn ran (stale-read rejections).",
	})

	turnsCancelled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "public_ai_turns_cancelled_total",
		Help: "Turns abandoned by the visitor mid-flight; the provider call is cancelled (Doc 05 §12).",
	})

	releaseReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "public_ai_release_ready",
		Help: "1 when verified knowledge and claim releases are active and fresh, else 0.",
	})
)

func init() {
	Registry.MustRegister(httpRequests, httpDuration, turns, turnDuration, modelRequests, modelTokens,
		rateLimited, eventsPublished, eventsDeadLettered, releaseReady, buildInfo,
		ctaOffered, handoffClicked, feedback, securitySignals, spendRatio, stageDuration, turnsCancelled, turnOutcomes, outboxOldest, commitRevocations,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}

// ObserveHTTP records one request. route must be a route pattern, never a
// raw path ("unmatched" when no route matched).
func ObserveHTTP(route, method string, code int, d time.Duration) {
	httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(route).Observe(d.Seconds())
}

// SetSpendRatio records usage/ceiling for window "day" or "month".
func SetSpendRatio(window string, ratio float64) { spendRatio.WithLabelValues(window).Set(ratio) }

// TurnOutcome counts a turn by status, release and market. market must be
// an offered market or "none" (bounded cardinality).
func TurnOutcome(status, release, market string) {
	turnOutcomes.WithLabelValues(status, release, market).Inc()
}

// OutboxOldest records the age of the oldest event in a relay batch.
func OutboxOldest(age time.Duration) { outboxOldest.Set(age.Seconds()) }

// CommitRevocation counts an answer withheld by the revocation check at commit.
func CommitRevocation() { commitRevocations.Inc() }

// TurnCancelled counts a turn the visitor cancelled.
func TurnCancelled() { turnsCancelled.Inc() }

// ObserveStage records one pipeline stage's duration.
func ObserveStage(stage string, d time.Duration) {
	stageDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// ObserveTurn records one completed turn.
func ObserveTurn(status, reason string, d time.Duration) {
	turns.WithLabelValues(status, reason).Inc()
	turnDuration.WithLabelValues(status).Observe(d.Seconds())
}

// ObserveModel records one model gateway call and its token usage.
func ObserveModel(result string, inputTokens, outputTokens int) {
	modelRequests.WithLabelValues(result).Inc()
	if inputTokens > 0 {
		modelTokens.WithLabelValues("input").Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		modelTokens.WithLabelValues("output").Add(float64(outputTokens))
	}
}

// RateLimited records a 429 for scope ip, session or inflight.
func RateLimited(scope string) { rateLimited.WithLabelValues(scope).Inc() }

// EventsPublished and EventsDeadLettered count outbox delivery outcomes.
func EventsPublished(n int)    { eventsPublished.Add(float64(n)) }
func EventsDeadLettered(n int) { eventsDeadLettered.Add(float64(n)) }

// CTAOffered, HandoffClicked, Feedback and SecuritySignal count conversion
// and safety outcomes; every label is a registry key or enum.
func CTAOffered(routeKey string)     { ctaOffered.WithLabelValues(routeKey).Inc() }
func HandoffClicked(routeKey string) { handoffClicked.WithLabelValues(routeKey).Inc() }
func Feedback(rating, reason string) {
	if reason == "" {
		reason = "none"
	}
	feedback.WithLabelValues(rating, reason).Inc()
}
func SecuritySignal(reasonClass string) { securitySignals.WithLabelValues(reasonClass).Inc() }

// SetBuildInfo records the app version and model profile.
func SetBuildInfo(gitSHA, modelProfile string) {
	buildInfo.Reset()
	buildInfo.WithLabelValues(gitSHA, modelProfile).Set(1)
}

// SetReleaseReady reports whether verified releases are active and fresh.
func SetReleaseReady(ready bool) {
	if ready {
		releaseReady.Set(1)
	} else {
		releaseReady.Set(0)
	}
}
