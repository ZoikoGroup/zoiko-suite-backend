// Package telemetry is workflow-history-svc's copy of this repo's
// Observability Baseline wiring (docs/architecture/03-microservices.md
// §3.8). Shaped as the Kafka-consumer variant (same as audit-event-store-svc):
// this service's primary work is consuming Kafka messages, not serving HTTP
// business requests. So instead of an HTTP request-count/duration pair,
// this exposes a messages-consumed counter (by topic, outcome), and traces
// a span per consumed message rather than per HTTP request.
// ReadinessUp is unchanged from every other copy — the ReadinessProbeFailing
// alert rule (deployments/prometheus-rules.yml) is shape-agnostic.
//
// Canonical HTTP-shaped copy: services/jurisdiction-rules-svc/internal/telemetry.
// This file is the Kafka-consumer-shaped variant — mirror consumer-shaped changes
// here and across audit-event-store-svc; mirror HTTP-shaped changes from the
// jurisdiction-rules-svc reference copy.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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

// Metrics holds this process's Prometheus collectors.
type Metrics struct {
	// MessagesConsumedTotal is this service's equivalent of every other
	// copy's HTTPRequestsTotal — one fact recorded per Kafka message
	// processed, regardless of outcome.
	MessagesConsumedTotal *prometheus.CounterVec
	ReadinessUp           prometheus.Gauge
}

func NewMetrics(serviceName string) *Metrics {
	m := &Metrics{
		MessagesConsumedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "messages_consumed_total",
			Help: "Total Kafka messages processed by this consumer. " +
				"outcome is store_error|ok — not broken down by event_type, " +
				"which Runner doesn't parse (that's Consumer.Handle's job).",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"topic", "outcome"}),
		ReadinessUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "readiness_up",
			Help:        "1 if the last /readyz check succeeded, 0 otherwise.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
	}
	prometheus.MustRegister(m.MessagesConsumedTotal, m.ReadinessUp)
	return m
}

// StartConsumeSpan starts one span per consumed Kafka message — this
// service's equivalent of otelchi's per-request span in the HTTP-shaped copies.
func StartConsumeSpan(ctx context.Context, topic, eventID string) (context.Context, trace.Span) {
	return otel.Tracer("workflow-history-svc").Start(ctx, "kafka.consume "+topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.message.id", eventID),
		),
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// WrapReadiness wraps a readiness handler to set ReadinessUp from the status
// code it writes — identical to every other copy's version.
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
