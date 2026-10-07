// Package telemetry is secret-vault-integration-svc's copy of this
// repo's Observability Baseline wiring (docs/architecture/
// 03-microservices.md §3.8: OpenTelemetry-compatible traces,
// service-level metrics, and an alertable failure-state signal).
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

	// ── Domain metrics ───────────────────────────────────────────────────────
	//
	// The three baseline families above describe the HTTP surface: how many
	// requests, how fast, and whether the process is ready. None of them can
	// answer the questions this service exists to answer. A burst of secret
	// access denials, an authorization-svc outage turning every mutation into
	// a fail-closed refusal, or a vault backend that has stopped returning
	// material all look identical in http_requests_total — some non-2xx
	// responses on a route — and two of those three are security events.
	//
	// Labelled by outcome rather than split into separate metric names so a
	// single query can express "what fraction of brokerage is being refused",
	// which is the alertable shape.

	// BrokerDecisionsTotal counts every terminal outcome of POST
	// /v1/secrets/broker: granted, denied, no_policy, vault_error, error.
	BrokerDecisionsTotal *prometheus.CounterVec

	// LeaseRevocationsTotal counts lease revocations by what caused them:
	// "explicit" (an operator revoked one lease) or "rotation" (material was
	// rotated and every lease on the path was invalidated). The two mean very
	// different things during an incident.
	LeaseRevocationsTotal *prometheus.CounterVec

	// RotationsTotal counts completed secret rotations, and
	// RotationRevokedLeases the leases they invalidated. Rotation is the
	// service's highest-consequence operation and had no signal at all.
	RotationsTotal        prometheus.Counter
	RotationRevokedLeases prometheus.Counter

	// AuthzDecisionsTotal counts authorization-svc outcomes per action:
	// allowed, denied, unavailable. "unavailable" is the one that matters
	// most — this service fails closed, so an authorization-svc outage
	// presents as a total write outage here, and without this label it is
	// indistinguishable from a wave of legitimate denials.
	AuthzDecisionsTotal *prometheus.CounterVec

	// VaultBackendErrorsTotal counts vault backend failures by operation
	// (get/put/rotate). A get failure means brokering is broken for material
	// that policy says should be reachable.
	VaultBackendErrorsTotal *prometheus.CounterVec
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
		BrokerDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "secret_vault_broker_decisions_total",
			Help:        "Terminal outcomes of secret brokerage requests, by outcome.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"outcome"}),
		LeaseRevocationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "secret_vault_lease_revocations_total",
			Help:        "Secret leases revoked, by cause (explicit or rotation).",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"cause"}),
		RotationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "secret_vault_rotations_total",
			Help:        "Completed secret rotations.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
		RotationRevokedLeases: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "secret_vault_rotation_revoked_leases_total",
			Help:        "Leases invalidated as a side effect of secret rotation.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}),
		AuthzDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "secret_vault_authz_decisions_total",
			Help:        "authorization-svc outcomes for this service's mutations, by action and outcome.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"action", "outcome"}),
		VaultBackendErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "secret_vault_backend_errors_total",
			Help:        "Vault backend failures, by operation.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"operation"}),
	}
	prometheus.MustRegister(
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.ReadinessUp,
		m.BrokerDecisionsTotal, m.LeaseRevocationsTotal,
		m.RotationsTotal, m.RotationRevokedLeases,
		m.AuthzDecisionsTotal, m.VaultBackendErrorsTotal,
	)
	// Counters are invisible in Prometheus until first incremented, so an
	// alert written against rate(...) over a label that has never been hit
	// evaluates against no series at all rather than against zero — it stays
	// silent through the exact outage it was written for. Initialising every
	// label combination at 0 makes the series exist from startup.
	for _, outcome := range []string{"granted", "denied", "no_policy", "vault_error", "error"} {
		m.BrokerDecisionsTotal.WithLabelValues(outcome)
	}
	for _, cause := range []string{"explicit", "rotation"} {
		m.LeaseRevocationsTotal.WithLabelValues(cause)
	}
	for _, op := range []string{"get", "put", "rotate"} {
		m.VaultBackendErrorsTotal.WithLabelValues(op)
	}
	for _, action := range []string{
		"SECRET_POLICY_CREATE", "SECRET_POLICY_VERSION_CREATE",
		"SECRET_POLICY_VERSION_ACTIVATE", "SECRET_MATERIAL_WRITE",
		"SECRET_LEASE_REVOKE", "SECRET_ROTATE",
	} {
		for _, outcome := range []string{"allowed", "denied", "unavailable"} {
			m.AuthzDecisionsTotal.WithLabelValues(action, outcome)
		}
	}
	return m
}

// ── handler.DomainMetrics implementation ─────────────────────────────────────
//
// The handler package declares the narrow interface these satisfy rather than
// importing this package, so handler tests need no Prometheus registry and a
// second registration of the same collector cannot happen in a test binary.

func (m *Metrics) BrokerDecision(outcome string) {
	m.BrokerDecisionsTotal.WithLabelValues(outcome).Inc()
}

func (m *Metrics) LeaseRevoked(cause string) {
	m.LeaseRevocationsTotal.WithLabelValues(cause).Inc()
}

func (m *Metrics) SecretRotated(revokedLeases int) {
	m.RotationsTotal.Inc()
	if revokedLeases > 0 {
		m.RotationRevokedLeases.Add(float64(revokedLeases))
		m.LeaseRevocationsTotal.WithLabelValues("rotation").Add(float64(revokedLeases))
	}
}

func (m *Metrics) AuthzDecision(action, outcome string) {
	m.AuthzDecisionsTotal.WithLabelValues(action, outcome).Inc()
}

func (m *Metrics) VaultBackendError(operation string) {
	m.VaultBackendErrorsTotal.WithLabelValues(operation).Inc()
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
