// Package telemetry is evidence-requirements-svc's copy of this repo's
// Observability Baseline wiring (docs/architecture/03-microservices.md
// §3.8: OpenTelemetry-compatible traces, service-level metrics, and an
// alertable failure-state signal).
//
// Canonical copy: services/jurisdiction-rules-svc/internal/telemetry —
// mirror changes there here, same convention as correlationIDMiddleware
// (copy-pasted per service, no shared Go module — every service's Docker
// build context is scoped to its own services/<svc> directory).
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
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
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

type Metrics struct {
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	ReadinessUp         prometheus.Gauge
}

func NewMetrics(serviceName string) *Metrics {
	m := &Metrics{
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "http_requests_total",
			Help:        "Total HTTP requests processed.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"method", "route", "status_code"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "http_request_duration_seconds",
			Help:        "HTTP request latency in seconds.",
			ConstLabels: prometheus.Labels{"service": serviceName},
			Buckets:     prometheus.DefBuckets,
		}, []string{"method", "route"}),
		ReadinessUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "readiness_up",
			Help:        "1 if the last /readyz check succeeded, 0 otherwise.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
	}
	prometheus.MustRegister(m.HTTPRequestsTotal, m.HTTPRequestDuration, m.ReadinessUp)
	return m
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
		status := strconv.Itoa(rec.status)
		m.HTTPRequestsTotal.WithLabelValues(r.Method, route, status).Inc()
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
// probes /healthz (liveness) and Prometheus scrapes /metrics. So the gauge
// would sit at its initial 0 forever and the ReadinessProbeFailing alert would
// fire for every healthy service. Evaluating readiness at scrape time makes
// the gauge reflect the service's actual current readiness, which is exactly
// what the alert needs.
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

// discardResponseWriter is a throwaway ResponseWriter used to run the readiness
// probe during a /metrics scrape without writing its body to the client.
type discardResponseWriter struct{ header http.Header }

func newDiscardResponseWriter() *discardResponseWriter {
	return &discardResponseWriter{header: make(http.Header)}
}
func (d *discardResponseWriter) Header() http.Header         { return d.header }
func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)             {}

// EvidenceMetrics are the domain series this service exports (ER-5):
//
//   - evidence_evaluations_total{outcome}: determinations recorded (replays
//     are not counted again);
//   - evidence_owner_unavailable_total{owner}: evaluations refused with 503
//     because an artifact owner could not answer (nothing was recorded);
//   - evidence_outbox_unpublished: events in the outbox not yet on Kafka.
//
// Every label value is created at zero, so an alert on rate() has a series to
// read before the first event.
type EvidenceMetrics struct {
	EvaluationsTotal      *prometheus.CounterVec
	OwnerUnavailableTotal *prometheus.CounterVec
	OutboxUnpublished     prometheus.Gauge
}

// NewEvidenceMetrics registers the domain series on reg.
func NewEvidenceMetrics(reg prometheus.Registerer, serviceName string) *EvidenceMetrics {
	m := &EvidenceMetrics{
		EvaluationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "evidence_evaluations_total",
			Help:        "Evidence determinations recorded, by outcome.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"outcome"}),
		OwnerUnavailableTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "evidence_owner_unavailable_total",
			Help:        "Evaluations refused (503, nothing recorded) because an artifact owner could not answer.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"owner"}),
		OutboxUnpublished: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "evidence_outbox_unpublished",
			Help:        "Events written to the outbox and not yet published to Kafka.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
	}
	reg.MustRegister(m.EvaluationsTotal, m.OwnerUnavailableTotal, m.OutboxUnpublished)
	for _, o := range []string{"SATISFIED", "MISSING", "NO_REQUIREMENTS_DEFINED"} {
		m.EvaluationsTotal.WithLabelValues(o)
	}
	for _, o := range []string{"document-vault-svc", "evidence-manifest-svc", "esignature-integration-svc"} {
		m.OwnerUnavailableTotal.WithLabelValues(o)
	}
	return m
}

// EvaluationRecorded counts one recorded determination.
func (m *EvidenceMetrics) EvaluationRecorded(outcome string) {
	m.EvaluationsTotal.WithLabelValues(outcome).Inc()
}

// OwnerUnavailable counts one evaluation refused because owner was down.
func (m *EvidenceMetrics) OwnerUnavailable(owner string) {
	m.OwnerUnavailableTotal.WithLabelValues(owner).Inc()
}

// SetOutboxUnpublished sets the outbox backlog gauge.
func (m *EvidenceMetrics) SetOutboxUnpublished(n int64) {
	m.OutboxUnpublished.Set(float64(n))
}
