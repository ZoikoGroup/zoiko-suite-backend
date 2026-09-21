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
// http_requests_total is not enough on this service, and the reason is the same
// one that made gateway-auth-svc invisible: every interesting thing that
// happens here is a well-formed refusal. A caller trying to mint themselves a
// colleague's authority gets a correct 403 and a correct log line, and an
// error-rate alert sees a service behaving perfectly. The counters below name
// the DECISION rather than the status code, so the difference between "nobody
// is attempting escalation" and "somebody is attempting it every minute" is
// visible at all.
type Domain struct {
	// Grants counts every attempt to create a delegation by what was decided.
	Grants *prometheus.CounterVec
	// Revocations counts every attempt to revoke, by outcome.
	Revocations *prometheus.CounterVec
	// RegisterReads counts register reads by the scope they were answered in —
	// entity-wide (DELEGATION_VIEW held) or self (the caller's own involvement).
	RegisterReads *prometheus.CounterVec
	// AuthZDecisions counts calls to authorization-svc by action and outcome.
	// "unavailable" is kept apart from "denied" because the two need opposite
	// responses: one is a permissions problem, the other a dependency outage.
	AuthZDecisions *prometheus.CounterVec
	// Expiries counts grants the lazy sweep flipped to EXPIRED. Expiry is
	// observed on read here, so this is also the only signal that reads are
	// happening often enough for the register to be current.
	Expiries prometheus.Counter
	// OutboxPending is the depth of the unpublished event backlog.
	OutboxPending prometheus.Gauge
	// OutboxPublished counts events the relay handed to Kafka.
	OutboxPublished *prometheus.CounterVec
	// OutboxFailures counts relay drain attempts that failed.
	OutboxFailures prometheus.Counter
	// OutboxOldestAgeSeconds is the age of the oldest unpublished event. Depth
	// alone cannot distinguish a busy moment from a stalled relay: a backlog of
	// ten that is three seconds old is healthy, and a backlog of ten that is an
	// hour old means authority.revoked has not reached the consumer that ends
	// the delegate's session.
	OutboxOldestAgeSeconds prometheus.Gauge
}

// Grant outcomes. Every one of these is a label value that must exist from
// startup — see NewDomain.
const (
	GrantCreated             = "created"
	GrantReplayed            = "replayed"
	GrantInvalidRequest      = "invalid_request"
	GrantDelegateIsDelegator = "delegate_is_delegator"
	GrantInvalidWindow       = "invalid_window"
	GrantNoCreateGrant       = "no_create_grant"
	GrantDelegatorMismatch   = "delegator_mismatch"
	GrantSelfDealing         = "self_dealing"
	GrantDelegatorLacksAuth  = "delegator_lacks_authority"
	GrantAuthzUnavailable    = "authz_unavailable"
	GrantStoreUnavailable    = "store_unavailable"
	GrantIdentityMissing     = "identity_missing"
	GrantTenantMissing       = "tenant_missing"
)

// Revocation outcomes.
const (
	RevokeRevoked         = "revoked"
	RevokeAlreadyTerminal = "already_terminal"
	RevokeForbidden       = "forbidden"
	RevokeNotFound        = "not_found"
	RevokeUnavailable     = "unavailable"
)

// Register read scopes.
const (
	ReadScopeEntity = "entity"
	ReadScopeSelf   = "self"
)

// AuthZ outcomes.
const (
	AuthZGranted     = "granted"
	AuthZDenied      = "denied"
	AuthZUnavailable = "unavailable"
)

// NewDomain registers this service's decision counters and pre-creates every
// label combination at zero.
//
// The pre-creation is the point, not housekeeping. A Prometheus series that has
// never been observed and a series reading zero are indistinguishable to an
// alert expression: `rate(...{outcome="self_dealing"}[5m]) > 0` evaluates
// against no data at all until the first self-dealing attempt, so a rule
// written to catch the first one stays silent through exactly the event it
// exists for. Same defect, same fix as gateway-auth-svc's outcome series.
func NewDomain(serviceName string) *Domain {
	return NewDomainWith(prometheus.DefaultRegisterer, serviceName)
}

// NewRegistry returns an isolated registry, for tests.
//
// It exists because prometheus.MustRegister panics on a duplicate collector
// name, so two tests that each want their own counters cannot both use the
// default registry — and a test that shares one with its neighbours is
// asserting on whatever ran before it.
func NewRegistry() *prometheus.Registry { return prometheus.NewRegistry() }

// NewDomainWith is NewDomain against a caller-supplied registry.
func NewDomainWith(reg prometheus.Registerer, serviceName string) *Domain {
	labels := prometheus.Labels{"service": serviceName}
	d := &Domain{
		Grants: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "delegated_authority_grants_total",
			Help:        "Delegation creation attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		Revocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "delegated_authority_revocations_total",
			Help:        "Delegation revocation attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		RegisterReads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "delegated_authority_register_reads_total",
			Help:        "Register reads by the scope they were answered in.",
			ConstLabels: labels,
		}, []string{"scope"}),
		AuthZDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "delegated_authority_authz_decisions_total",
			Help:        "Calls to authorization-svc by action and outcome.",
			ConstLabels: labels,
		}, []string{"action", "outcome"}),
		Expiries: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "delegated_authority_expiries_total",
			Help:        "Delegations flipped to EXPIRED by the lazy sweep.",
			ConstLabels: labels,
		}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "delegated_authority_outbox_pending",
			Help:        "Events written but not yet published to Kafka.",
			ConstLabels: labels,
		}),
		OutboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "delegated_authority_outbox_published_total",
			Help:        "Events published from the outbox, by event type.",
			ConstLabels: labels,
		}, []string{"event_type"}),
		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "delegated_authority_outbox_failures_total",
			Help:        "Outbox drain attempts that failed.",
			ConstLabels: labels,
		}),
		OutboxOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "delegated_authority_outbox_oldest_age_seconds",
			Help:        "Age of the oldest unpublished outbox event, in seconds.",
			ConstLabels: labels,
		}),
	}
	reg.MustRegister(
		d.Grants, d.Revocations, d.RegisterReads, d.AuthZDecisions, d.Expiries,
		d.OutboxPending, d.OutboxPublished, d.OutboxFailures, d.OutboxOldestAgeSeconds,
	)

	for _, o := range []string{
		GrantCreated, GrantReplayed, GrantInvalidRequest, GrantDelegateIsDelegator,
		GrantInvalidWindow, GrantNoCreateGrant, GrantDelegatorMismatch, GrantSelfDealing,
		GrantDelegatorLacksAuth, GrantAuthzUnavailable, GrantStoreUnavailable,
		GrantIdentityMissing, GrantTenantMissing,
	} {
		d.Grants.WithLabelValues(o)
	}
	for _, o := range []string{RevokeRevoked, RevokeAlreadyTerminal, RevokeForbidden, RevokeNotFound, RevokeUnavailable} {
		d.Revocations.WithLabelValues(o)
	}
	for _, s := range []string{ReadScopeEntity, ReadScopeSelf} {
		d.RegisterReads.WithLabelValues(s)
	}
	for _, a := range []string{"DELEGATION_CREATE", "DELEGATION_VIEW", "DELEGATION_REVOKE", "DELEGATION_ADMINISTER", "DELEGATED_ACTION"} {
		for _, o := range []string{AuthZGranted, AuthZDenied, AuthZUnavailable} {
			d.AuthZDecisions.WithLabelValues(a, o)
		}
	}
	for _, e := range []string{"authority.delegated", "authority.revoked", "authority.expired"} {
		d.OutboxPublished.WithLabelValues(e)
	}
	return d
}
