// Package telemetry is search-indexer-svc's copy of this repo's Observability
// Baseline wiring (docs/architecture/03-microservices.md §3.8), extended with
// the signals ZS-SVC-AB-001 §13.1 makes mandatory for a search plane.
//
// Canonical copy: services/jurisdiction-rules-svc/internal/telemetry — mirror
// generic changes there here. Everything below the HTTP block is specific to
// this service and has no counterpart there.
//
// §13.1 asks for seven signal families, and the one that shapes this file is
// the separation of FRESHNESS from RESTRICTION LAG:
//
//	"The architecture requires separate normal-indexing freshness and
//	 visibility-reduction propagation objectives so revocation is not hidden
//	 inside a generic 'eventual consistency' SLA."
//
// So IndexLagSeconds and RestrictionLagSeconds are two histograms, not one
// with a label. A label would let an alert be written against the aggregate,
// and the aggregate is dominated by ordinary indexing — which is exactly how a
// revocation that has not propagated stays invisible behind a healthy average.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

func InitTracing(ctx context.Context, serviceName, otlpEndpoint string) (func(context.Context) error, error) {
	endpoint := strings.TrimPrefix(strings.TrimPrefix(otlpEndpoint, "https://"), "http://")

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create OTLP trace exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// StartConsumeSpan opens a span for one consumed Kafka message.
//
// The event id is an attribute, never the payload. A domain event's payload is
// the tenant's business data, and a trace backend is not a place it belongs —
// the same minimisation §9.2 applies to query text applies here.
func StartConsumeSpan(ctx context.Context, topic, eventID string) (context.Context, trace.Span) {
	return otel.Tracer("search-indexer-svc").Start(ctx, "consume "+topic,
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.source.name", topic),
			attribute.String("messaging.message.id", eventID),
		),
	)
}

type Metrics struct {
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	ReadinessUp         prometheus.Gauge

	// ── §13.1 Completeness / freshness ───────────────────────────────────
	MessagesConsumedTotal *prometheus.CounterVec
	ProjectionsTotal      *prometheus.CounterVec
	IndexLagSeconds       *prometheus.HistogramVec

	// ── §13.1 Restriction lag ────────────────────────────────────────────
	RestrictionsTotal     *prometheus.CounterVec
	RestrictionLagSeconds prometheus.Histogram
	// RestrictionBacklog is a GAUGE, not a counter. §8.2 wants the CURRENT
	// unverified population — "if verification fails beyond threshold,
	// affected search scope can be blocked/degraded" is a decision about now,
	// not about a rate.
	RestrictionBacklog *prometheus.GaugeVec

	// ── §13.1 Authorization outcomes ─────────────────────────────────────
	RetrievalDecisionsTotal *prometheus.CounterVec
	AuthzDecisionsTotal     *prometheus.CounterVec

	// ── §13.1 Search quality and security abuse ──────────────────────────
	SearchesTotal        *prometheus.CounterVec
	SearchDurationSecs   *prometheus.HistogramVec
	SearchZeroResults    *prometheus.CounterVec
	QueryRejectionsTotal *prometheus.CounterVec

	// ── §13.1 Reindex certification / engine health ──────────────────────
	GenerationTransitions *prometheus.CounterVec
	EngineErrorsTotal     *prometheus.CounterVec
}

func NewMetrics(serviceName string) *Metrics {
	labels := prometheus.Labels{"service": serviceName}
	m := &Metrics{
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "Total HTTP requests processed.",
			ConstLabels: labels,
		}, []string{"method", "route", "status_code"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency in seconds.",
			ConstLabels: labels, Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		ReadinessUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "readiness_up", Help: "1 if the last /readyz check succeeded, 0 otherwise.",
			ConstLabels: labels,
		}),

		MessagesConsumedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_messages_consumed_total",
			Help: "Domain events consumed, by topic and outcome.", ConstLabels: labels,
		}, []string{"topic", "outcome"}),
		ProjectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_projections_total",
			Help: "Projection write outcomes, by scope and outcome.", ConstLabels: labels,
		}, []string{"scope", "outcome"}),
		IndexLagSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "search_indexer_index_lag_seconds",
			Help:        "Source-event-to-index lag, by scope (§13.1 freshness).",
			ConstLabels: labels,
			// Wide buckets: an S0 reference corpus and an S3 privileged one
			// live in the same histogram, and their acceptable lags differ by
			// three orders of magnitude (OD-03).
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 900, 3600},
		}, []string{"scope"}),

		RestrictionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_restrictions_total",
			Help: "Restriction/tombstone outcomes, by scope and state.", ConstLabels: labels,
		}, []string{"scope", "state"}),
		RestrictionLagSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "search_indexer_restriction_lag_seconds",
			Help: "Time from authoritative restriction event to PROVEN invisibility (§13.1). " +
				"This is the over-disclosure exposure window and is deliberately " +
				"separate from index lag.",
			ConstLabels: labels,
			Buckets:     []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 900},
		}),
		RestrictionBacklog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "search_indexer_restriction_backlog",
			Help:        "Restrictions applied but not yet verified invisible, by scope.",
			ConstLabels: labels,
		}, []string{"scope"}),

		RetrievalDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "search_indexer_retrieval_decisions_total",
			Help:        "Per-result retrieval outcomes, by scope and outcome (§13.1).",
			ConstLabels: labels,
		}, []string{"scope", "outcome"}),
		AuthzDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_authz_decisions_total",
			Help: "authorization-svc outcomes, by action and outcome.", ConstLabels: labels,
		}, []string{"action", "outcome"}),

		SearchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_searches_total",
			Help: "Executed searches, by scope and completeness state.", ConstLabels: labels,
		}, []string{"scope", "completeness"}),
		SearchDurationSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "search_indexer_search_duration_seconds",
			Help:        "End-to-end governed search latency, by scope.",
			ConstLabels: labels, Buckets: prometheus.DefBuckets,
		}, []string{"scope"}),
		SearchZeroResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "search_indexer_zero_result_searches_total",
			Help:        "Searches that returned nothing, by scope (§13.1 search quality).",
			ConstLabels: labels,
		}, []string{"scope"}),
		QueryRejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "search_indexer_query_rejections_total",
			Help:        "Queries refused by the planner, by ESR reason code (§13.1 security abuse).",
			ConstLabels: labels,
		}, []string{"scope", "reason_code"}),

		GenerationTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "search_indexer_generation_transitions_total",
			Help:        "Index generation state transitions, by scope and target state.",
			ConstLabels: labels,
		}, []string{"scope", "state"}),
		EngineErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "search_indexer_engine_errors_total",
			Help: "OpenSearch failures, by operation.", ConstLabels: labels,
		}, []string{"operation"}),
	}

	prometheus.MustRegister(
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.ReadinessUp,
		m.MessagesConsumedTotal, m.ProjectionsTotal, m.IndexLagSeconds,
		m.RestrictionsTotal, m.RestrictionLagSeconds, m.RestrictionBacklog,
		m.RetrievalDecisionsTotal, m.AuthzDecisionsTotal,
		m.SearchesTotal, m.SearchDurationSecs, m.SearchZeroResults, m.QueryRejectionsTotal,
		m.GenerationTransitions, m.EngineErrorsTotal,
	)

	// Counters are invisible in Prometheus until first incremented, so an
	// alert written against rate(...) over a label that has never been hit
	// evaluates against no series at all rather than against zero — it stays
	// silent through the exact outage it was written for. Initialising the
	// label combinations that matter makes the series exist from startup.
	//
	// The retrieval outcomes are the ones this most matters for: a
	// SUPPRESS-rate alert is meaningless if the series only appears the first
	// time something is suppressed.
	for _, outcome := range []string{"permit", "suppress", "indeterminate", "review_required"} {
		m.RetrievalDecisionsTotal.WithLabelValues("global", outcome)
	}
	for _, outcome := range []string{"ok", "quarantined", "stale", "error", "dead_lettered"} {
		m.MessagesConsumedTotal.WithLabelValues("bootstrap", outcome)
	}
	for _, state := range []string{"PENDING", "APPLIED", "VERIFIED", "FAILED"} {
		m.RestrictionsTotal.WithLabelValues("global", state)
	}
	for _, op := range []string{"index", "delete", "search", "alias", "create", "count"} {
		m.EngineErrorsTotal.WithLabelValues(op)
	}
	// A *Vec with no observed label values exposes NOTHING on /metrics — not a
	// zero, no series at all. These two are the ones that stay empty longest in
	// practice: index lag and projection outcomes are only written once events
	// start flowing, so a freshly deployed scope shows neither, and a dashboard
	// panel or an alert written against them evaluates against nothing rather
	// than against zero. "bootstrap" is a placeholder scope label, replaced by
	// real ones the moment anything is indexed.
	m.IndexLagSeconds.WithLabelValues("bootstrap")
	for _, outcome := range []string{"ok", "skipped", "stale", "quarantined", "error"} {
		m.ProjectionsTotal.WithLabelValues("bootstrap", outcome)
	}
	for _, completeness := range []string{"COMPLETE", "PARTIAL", "DEGRADED", "UNKNOWN"} {
		m.SearchesTotal.WithLabelValues("bootstrap", completeness)
	}
	m.SearchDurationSecs.WithLabelValues("bootstrap")
	m.SearchZeroResults.WithLabelValues("bootstrap")
	// The refusal codes an abuse alert would actually be written against.
	// A rate() over a code that has never fired evaluates against no series at
	// all rather than against zero — silent through exactly the enumeration
	// attempt it exists to catch.
	for _, code := range []string{"ESR-003", "ESR-004", "ESR-005", "ESR-006", "ESR-015"} {
		m.QueryRejectionsTotal.WithLabelValues("bootstrap", code)
	}
	for _, state := range []string{"BUILDING", "VALIDATING", "READY", "ACTIVE", "FAILED", "RETIRED"} {
		m.GenerationTransitions.WithLabelValues("bootstrap", state)
	}
	return m
}

func (m *Metrics) AuthzDecision(action, outcome string) {
	m.AuthzDecisionsTotal.WithLabelValues(action, outcome).Inc()
}

func (m *Metrics) EngineError(operation string) {
	m.EngineErrorsTotal.WithLabelValues(operation).Inc()
}

// ObserveRestrictionVerified records one proven-invisible restriction and the
// window it was exposed for.
func (m *Metrics) ObserveRestrictionVerified(scope string, effectiveAt time.Time) {
	m.RestrictionsTotal.WithLabelValues(scope, "VERIFIED").Inc()
	if !effectiveAt.IsZero() {
		m.RestrictionLagSeconds.Observe(time.Since(effectiveAt).Seconds())
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		m.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		m.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

func (m *Metrics) WrapReadiness(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if rec.status == http.StatusOK {
			m.ReadinessUp.Set(1)
		} else {
			m.ReadinessUp.Set(0)
		}
	}
}

// MetricsHandler wraps the Prometheus scrape endpoint so that every scrape
// first re-evaluates readiness and refreshes the readiness_up gauge.
//
// Without this, readiness_up only ever updates when something calls /readyz —
// but nothing in this platform does on a schedule: the Docker healthcheck
// probes /healthz and Prometheus scrapes /metrics. The gauge would sit at its
// initial 0 forever and the ReadinessProbeFailing alert would fire for every
// healthy service.
func (m *Metrics) MetricsHandler(readyz http.HandlerFunc, promHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: newDiscardResponseWriter(), status: http.StatusOK}
		readyz(rec, r)
		if rec.status == http.StatusOK {
			m.ReadinessUp.Set(1)
		} else {
			m.ReadinessUp.Set(0)
		}
		promHandler.ServeHTTP(w, r)
	})
}

type discardResponseWriter struct{ header http.Header }

func newDiscardResponseWriter() *discardResponseWriter {
	return &discardResponseWriter{header: make(http.Header)}
}
func (d *discardResponseWriter) Header() http.Header         { return d.header }
func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)             {}
