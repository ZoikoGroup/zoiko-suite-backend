// Package telemetry is gateway-auth-svc's copy of this repo's Observability
// Baseline wiring (docs/architecture/03-microservices.md §3.8:
// OpenTelemetry-compatible traces, service-level metrics, and an alertable
// failure-state signal).
//
// Canonical copy: services/jurisdiction-rules-svc/internal/telemetry — mirror
// changes there here, same convention as every other service (copy-pasted per
// service, no shared Go module: each Docker build context is scoped to its own
// services/<svc> directory).
//
// This service had NO telemetry of any kind until this package existed — no
// /metrics endpoint, no Prometheus client dependency, no scrape job and no
// alert rules. It is the ForwardAuth target for every gated request in the
// estate, so it is both the first thing to fail and the least visible: when it
// refuses, the symptom an operator sees is every OTHER service answering 401,
// and nothing anywhere recorded why.
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
	// requests, how fast, and whether the process is ready. On this service
	// they are close to useless alone, because /verify is ONE route and every
	// interesting outcome — a forged token, a suspended tenant, a registry
	// outage, a spoofing attempt — arrives as a non-2xx on that same route. In
	// http_requests_total they are one indistinguishable bucket.
	//
	// Worse, several of them are not failures at all from the gateway's point
	// of view: refusing a request that carried no bearer token is this service
	// working exactly as designed, and it shares its status code with a JWKS
	// outage that is breaking every login in the estate. Labelling by outcome
	// is what separates "the gateway is doing its job" from "the gateway is
	// down", and those two need opposite responses.

	// VerifyDecisionsTotal counts every terminal outcome of /verify. The
	// service's primary signal: the ratio of allowed to each refusal reason is
	// the only way to tell a credential problem from an outage.
	VerifyDecisionsTotal *prometheus.CounterVec

	// JWKSErrorsTotal counts failures to obtain a verification key, by
	// operation: "fetch" (the JWKS endpoint could not be read) or "key_lookup"
	// (it was read and did not contain the token's kid).
	//
	// Separated because they fail differently. A fetch failure is an
	// identity-context-svc outage and blocks EVERY request; a key_lookup miss
	// is usually a key rotation this gateway's cache has not caught up with,
	// and blocks only tokens signed by the new key.
	JWKSErrorsTotal *prometheus.CounterVec

	// TenantContextTotal counts GOV-01 resolution outcomes: resolved, stale,
	// denied, unavailable, disabled.
	//
	// "denied" and "unavailable" both refuse the request and mean opposite
	// things — the registry made a decision, versus no decision could be
	// obtained — the same distinction the response carries in X-Tenant-Context.
	// "stale" is a bounded-grace read served from a cache the registry could
	// not confirm; a rising stale rate is the early warning that "unavailable"
	// is coming.
	TenantContextTotal *prometheus.CounterVec

	// CartaDecisionsTotal counts continuous-risk assessments by decision.
	// STEP_UP_MFA is counted but deliberately not enforced — there is no
	// step-up flow downstream to redirect to — so this counter is the only
	// place that signal is visible at all.
	CartaDecisionsTotal *prometheus.CounterVec
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
		VerifyDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gateway_auth_verify_decisions_total",
			Help:        "Terminal outcomes of ForwardAuth /verify, by outcome.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"outcome"}),
		JWKSErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gateway_auth_jwks_errors_total",
			Help:        "Failures obtaining a JWKS verification key, by operation.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"operation"}),
		TenantContextTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gateway_auth_tenant_context_total",
			Help:        "GOV-01 tenant context resolution outcomes.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"outcome"}),
		CartaDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gateway_auth_carta_decisions_total",
			Help:        "Continuous risk assessment decisions, by decision.",
			ConstLabels: prometheus.Labels{"service": serviceName},
		}, []string{"decision"}),
	}
	prometheus.MustRegister(
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.ReadinessUp,
		m.VerifyDecisionsTotal, m.JWKSErrorsTotal,
		m.TenantContextTotal, m.CartaDecisionsTotal,
	)
	// Counters are invisible in Prometheus until first incremented, so an alert
	// written against rate(...) over a label that has never been hit evaluates
	// against no series at all rather than against zero — it stays silent
	// through the exact outage it was written for. Initialising every label
	// combination at 0 makes the series exist from startup.
	//
	// That matters more here than on a typical service: the labels that should
	// stay at zero forever are the security ones, and "no series" and "no
	// spoofing attempts" would otherwise look identical.
	for _, outcome := range []string{
		OutcomeAllowed, OutcomeNoToken, OutcomeInvalidToken, OutcomeIncompleteClaims,
		OutcomeTenantHostnameMismatch, OutcomeCartaBlocked,
		OutcomeTenantContextDenied, OutcomeTenantContextUnresolved,
	} {
		m.VerifyDecisionsTotal.WithLabelValues(outcome)
	}
	for _, op := range []string{"fetch", "key_lookup"} {
		m.JWKSErrorsTotal.WithLabelValues(op)
	}
	for _, outcome := range []string{"resolved", "stale", "denied", "unavailable", "disabled"} {
		m.TenantContextTotal.WithLabelValues(outcome)
	}
	for _, d := range []string{"ALLOW", "STEP_UP_MFA", "ISOLATE", "DENY"} {
		m.CartaDecisionsTotal.WithLabelValues(d)
	}
	return m
}

// Terminal outcomes of /verify. Constants rather than string literals at each
// call site so the set the handler emits and the set pre-initialised above
// cannot drift — a typo at one call site would otherwise create a silent ninth
// series that no alert selects.
const (
	// OutcomeAllowed — verified, resolved, forwarded.
	OutcomeAllowed = "allowed"
	// OutcomeNoToken — no bearer credential at all. Ordinary background noise
	// on a public gateway; alarming only as a sudden change in ratio.
	OutcomeNoToken = "no_token"
	// OutcomeInvalidToken — signature, expiry, issuer, audience or kid failed.
	OutcomeInvalidToken = "invalid_token"
	// OutcomeIncompleteClaims — the token verified but carries no principal or
	// no tenant. Points at identity-context-svc minting a malformed envelope,
	// not at the caller.
	OutcomeIncompleteClaims = "incomplete_claims"
	// OutcomeTenantHostnameMismatch — a validly-signed token presented against
	// a different tenant's hostname. The one outcome here that is evidence of
	// an attack rather than of a misconfiguration.
	OutcomeTenantHostnameMismatch = "tenant_hostname_mismatch"
	// OutcomeCartaBlocked — continuous risk assessment returned ISOLATE or DENY.
	OutcomeCartaBlocked = "carta_blocked"
	// OutcomeTenantContextDenied — the registry refused: unknown or suspended
	// tenant, or a legal entity belonging to someone else.
	OutcomeTenantContextDenied = "tenant_context_denied"
	// OutcomeTenantContextUnresolved — the registry could not be reached, so no
	// decision exists. Fail-closed, and an outage rather than a refusal.
	OutcomeTenantContextUnresolved = "tenant_context_unresolved"
)

// ── handler.DomainMetrics implementation ─────────────────────────────────────
//
// The handler package declares the narrow interface these satisfy rather than
// importing this package, so handler tests need no Prometheus registry and a
// second registration of the same collector cannot happen in a test binary.

func (m *Metrics) VerifyDecision(outcome string) {
	m.VerifyDecisionsTotal.WithLabelValues(outcome).Inc()
}

func (m *Metrics) JWKSError(operation string) {
	m.JWKSErrorsTotal.WithLabelValues(operation).Inc()
}

func (m *Metrics) TenantContext(outcome string) {
	m.TenantContextTotal.WithLabelValues(outcome).Inc()
}

func (m *Metrics) CartaDecision(decision string) {
	m.CartaDecisionsTotal.WithLabelValues(decision).Inc()
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
