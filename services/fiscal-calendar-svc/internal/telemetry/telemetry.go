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

// ── Domain metrics ───────────────────────────────────────────────────────────
//
// http_requests_total is not enough here: most of what this service decides is
// a well-formed refusal (a quarantined import, a SoD denial, a stale
// expected_version) that an error-rate alert never sees. These counters name the
// DECISION, and every label value is pre-created at zero so an alert written
// against the first occurrence has a series to evaluate.
type Domain struct {
	// Commands counts named commands by command and outcome.
	Commands *prometheus.CounterVec
	// Resolutions counts calendar resolve calls by result (resolved / not_found).
	Resolutions *prometheus.CounterVec
	// AuthZDecisions counts calls to authorization-svc by action and outcome.
	AuthZDecisions *prometheus.CounterVec
	// OutboxPending is the depth of the unpublished event backlog.
	OutboxPending prometheus.Gauge
	// OutboxPublished counts events the relay handed to Kafka.
	OutboxPublished *prometheus.CounterVec
	// OutboxFailures counts relay drain attempts that failed.
	OutboxFailures prometheus.Counter
	// OutboxOldestAgeSeconds is the age of the oldest unpublished event. Depth
	// alone cannot distinguish a busy moment from a stalled relay.
	OutboxOldestAgeSeconds prometheus.Gauge
}

// Command names.
const (
	CmdCreate          = "create_calendar"
	CmdProposeChange   = "propose_change"
	CmdApproveVersion  = "approve_version"
	CmdActivateVersion = "activate_version"
	CmdCreatePlan      = "create_transition_plan"
	CmdApprovePlan     = "approve_transition_plan"
)

// Outcomes are the typed error codes plus success/replay, so a dashboard reads
// the same vocabulary the API speaks.
const (
	OutcomeOK          = "ok"
	OutcomeReplayed    = "replayed"
	OutcomeQuarantined = "quarantined"
	OutcomeRefused     = "refused"
	OutcomeForbidden   = "forbidden"
	OutcomeUnavailable = "unavailable"
	OutcomeInvalid     = "invalid"
)

// AuthZ outcomes.
const (
	AuthZGranted     = "granted"
	AuthZDenied      = "denied"
	AuthZUnavailable = "unavailable"
)

// NewDomain registers this service's decision counters on the default registry.
func NewDomain(serviceName string) *Domain {
	return NewDomainWith(prometheus.DefaultRegisterer, serviceName)
}

// NewRegistry returns an isolated registry, for tests.
func NewRegistry() *prometheus.Registry { return prometheus.NewRegistry() }

// NewDomainWith is NewDomain against a caller-supplied registry.
func NewDomainWith(reg prometheus.Registerer, serviceName string) *Domain {
	labels := prometheus.Labels{"service": serviceName}
	d := &Domain{
		Commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fiscal_calendar_commands_total", Help: "Named commands by command and outcome.", ConstLabels: labels,
		}, []string{"command", "outcome"}),
		Resolutions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fiscal_calendar_resolutions_total", Help: "Calendar resolve calls by result.", ConstLabels: labels,
		}, []string{"result"}),
		AuthZDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fiscal_calendar_authz_decisions_total", Help: "Calls to authorization-svc by action and outcome.", ConstLabels: labels,
		}, []string{"action", "outcome"}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fiscal_calendar_outbox_pending", Help: "Events written but not yet published to Kafka.", ConstLabels: labels,
		}),
		OutboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fiscal_calendar_outbox_published_total", Help: "Events published from the outbox, by event type.", ConstLabels: labels,
		}, []string{"event_type"}),
		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fiscal_calendar_outbox_failures_total", Help: "Outbox drain attempts that failed.", ConstLabels: labels,
		}),
		OutboxOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fiscal_calendar_outbox_oldest_age_seconds", Help: "Age of the oldest unpublished outbox event, in seconds.", ConstLabels: labels,
		}),
	}
	reg.MustRegister(d.Commands, d.Resolutions, d.AuthZDecisions,
		d.OutboxPending, d.OutboxPublished, d.OutboxFailures, d.OutboxOldestAgeSeconds)

	for _, c := range []string{CmdCreate, CmdProposeChange, CmdApproveVersion, CmdActivateVersion, CmdCreatePlan, CmdApprovePlan} {
		for _, o := range []string{OutcomeOK, OutcomeReplayed, OutcomeRefused, OutcomeForbidden, OutcomeUnavailable, OutcomeInvalid} {
			d.Commands.WithLabelValues(c, o)
		}
	}
	for _, r := range []string{"resolved", "not_found"} {
		d.Resolutions.WithLabelValues(r)
	}
	for _, a := range []string{"FISCAL_CALENDAR_PROPOSE", "FISCAL_CALENDAR_APPROVE", "FISCAL_CALENDAR_ACTIVATE"} {
		for _, o := range []string{AuthZGranted, AuthZDenied, AuthZUnavailable} {
			d.AuthZDecisions.WithLabelValues(a, o)
		}
	}
	for _, e := range []string{"FiscalCalendarCreated", "FiscalCalendarChangeProposed", "FiscalCalendarVersionActivated", "FiscalCalendarSuperseded"} {
		d.OutboxPublished.WithLabelValues(e)
	}
	return d
}
