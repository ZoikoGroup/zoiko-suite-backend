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

	// Delivery latency and attempt counts (ZS-COMMS-EMAIL-001 §14)
	DeliveryDuration      *prometheus.HistogramVec
	DeliveryAttemptsTotal *prometheus.CounterVec

	// Stream breakdown and message intent rates
	DeliveryIntentsTotal *prometheus.CounterVec

	// Webhook intake rates (bounce, complaint, delivered)
	WebhookEventsTotal *prometheus.CounterVec

	// Webhook DLQ volume
	WebhookDLQTotal *prometheus.CounterVec
}

func NewMetrics(serviceName string) *Metrics {
	return NewMetricsWithRegisterer(serviceName, prometheus.DefaultRegisterer)
}

func NewMetricsWithRegisterer(serviceName string, reg prometheus.Registerer) *Metrics {
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
		DeliveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "notification_delivery_duration_seconds",
			Help:        "Latency of delivery transport attempts in seconds.",
			ConstLabels: prometheus.Labels{"service": serviceName},
			Buckets:     []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"stream", "provider", "status"}),
		DeliveryAttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_delivery_attempts_total",
			Help:        "Total delivery attempts dispatched by stream, provider, and outcome status.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"stream", "provider", "status"}),
		DeliveryIntentsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_intents_total",
			Help:        "Total notification message intents processed by stream, class, and status.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"stream", "comm_class", "status"}),
		WebhookEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_webhook_events_total",
			Help:        "Total webhook delivery outcome events received from mail providers.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"provider", "event_type"}),
		WebhookDLQTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notification_webhook_dlq_total",
			Help:        "Total webhook events routed to or reprocessed from dead letter queue.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"provider", "status"}),
	}
	if reg != nil {
		reg.MustRegister(
			m.HTTPRequestsTotal,
			m.HTTPRequestDuration,
			m.ReadinessUp,
			m.DeliveryDuration,
			m.DeliveryAttemptsTotal,
			m.DeliveryIntentsTotal,
			m.WebhookEventsTotal,
			m.WebhookDLQTotal,
		)
	}
	return m
}

// ObserveDelivery records delivery attempt outcome and transport duration.
func (m *Metrics) ObserveDelivery(stream, provider, status string, durationSec float64) {
	if m == nil || m.DeliveryDuration == nil || m.DeliveryAttemptsTotal == nil {
		return
	}
	if stream == "" {
		stream = "UNKNOWN"
	}
	if provider == "" {
		provider = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	m.DeliveryDuration.WithLabelValues(stream, provider, status).Observe(durationSec)
	m.DeliveryAttemptsTotal.WithLabelValues(stream, provider, status).Inc()
}

// RecordIntent records a processed message intent.
func (m *Metrics) RecordIntent(stream, commClass, status string) {
	if m == nil || m.DeliveryIntentsTotal == nil {
		return
	}
	if stream == "" {
		stream = "UNKNOWN"
	}
	if commClass == "" {
		commClass = "UNKNOWN"
	}
	if status == "" {
		status = "unknown"
	}
	m.DeliveryIntentsTotal.WithLabelValues(stream, commClass, status).Inc()
}

// RecordWebhookEvent records an incoming webhook event by provider and type (e.g. bounce, complaint).
func (m *Metrics) RecordWebhookEvent(provider, eventType string) {
	if m == nil || m.WebhookEventsTotal == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	if eventType == "" {
		eventType = "unknown"
	}
	m.WebhookEventsTotal.WithLabelValues(provider, eventType).Inc()
}

// RecordDLQ records an item routed to, reprocessed from, or abandoned in the DLQ.
func (m *Metrics) RecordDLQ(provider, status string) {
	if m == nil || m.WebhookDLQTotal == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	m.WebhookDLQTotal.WithLabelValues(provider, status).Inc()
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
