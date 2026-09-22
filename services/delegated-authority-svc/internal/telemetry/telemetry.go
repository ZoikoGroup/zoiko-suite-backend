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
	// Expiries counts grants flipped to EXPIRED, by either sweep path.
	Expiries prometheus.Counter
	// ExpiryLatenessSeconds is how long each expired grant outlived its own
	// effective_to before the sweep ended it.
	//
	// This is the one number that says whether expiry is working, and none of
	// the others can substitute. A service expiring plenty of grants, every one
	// of them hours late, is busy by every other measure and healthy by none:
	// each of those hours is a delegate still holding authority the register
	// says has lapsed. On a healthy service this is bounded by the sweeper's
	// poll interval.
	ExpiryLatenessSeconds prometheus.Histogram
	// ExpiryDuePending is how many ACTIVE grants are currently past their
	// window across every tenant — the sweep's backlog.
	ExpiryDuePending prometheus.Gauge
	// ExpiryOldestOverdueSeconds is how overdue the oldest unexpired grant is.
	// Depth alone cannot tell a tick that caught a burst from a sweeper that
	// has stopped making progress.
	ExpiryOldestOverdueSeconds prometheus.Gauge
	// ExpirySweepFailures counts background sweep passes that failed. Distinct
	// from the read-path sweep, whose errors are logged and swallowed so they
	// cannot fail the read they piggyback on — which means this counter is the
	// only place a persistently failing sweep becomes visible.
	ExpirySweepFailures prometheus.Counter
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
			Help:        "Delegations flipped to EXPIRED, by either sweep path.",
			ConstLabels: labels,
		}),
		ExpiryLatenessSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "delegated_authority_expiry_lateness_seconds",
			Help: "How long a delegation outlived its effective_to before being expired.",
			// Buckets chosen around the failure this metric exists to catch.
			// The sweeper polls every 30s, so anything up to ~60s is the
			// mechanism working. The interesting resolution is above that, and
			// it runs to a day because the defect being fixed here -- expiry
			// waiting for somebody to read the register -- produced lateness
			// measured in weekends, not seconds.
			Buckets:     []float64{1, 5, 15, 30, 60, 300, 900, 3600, 21600, 86400},
			ConstLabels: labels,
		}),
		ExpiryDuePending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "delegated_authority_expiry_due_pending",
			Help:        "ACTIVE delegations currently past their effective_to, across all tenants.",
			ConstLabels: labels,
		}),
		ExpiryOldestOverdueSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "delegated_authority_expiry_oldest_overdue_seconds",
			Help:        "Age of the most overdue unexpired delegation.",
			ConstLabels: labels,
		}),
		ExpirySweepFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "delegated_authority_expiry_sweep_failures_total",
			Help:        "Background expiry sweep passes that failed.",
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
		d.ExpiryLatenessSeconds, d.ExpiryDuePending, d.ExpiryOldestOverdueSeconds,
		d.ExpirySweepFailures,
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
