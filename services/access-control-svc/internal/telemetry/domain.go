package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Domain holds the counters that describe what this service DECIDED, as
// distinct from the HTTP counters in telemetry.go which describe what it
// answered.
//
// The distinction is the point. Every interesting failure on this service
// produces a perfectly ordinary-looking response: a role write refused because
// the action name does not match any grant is a 403 like any other, and a
// retirement that could not be propagated to authorization-svc is a 503 that
// reads like a dependency blip. `http_requests_total{status_code="403"}` cannot
// tell either of those from a user clicking something they should not have.
type Domain struct {
	// RoleWrites counts every create/update attempt on a role definition by
	// outcome.
	RoleWrites *prometheus.CounterVec
	// BundleWrites counts every create/update/detach attempt on a permission
	// bundle by outcome.
	BundleWrites *prometheus.CounterVec
	// AuthZDecisions counts calls to authorization-svc's /v1/authorize by
	// outcome. "unavailable" is kept apart from "denied" because the two need
	// opposite responses: one is a permissions problem, the other an outage.
	AuthZDecisions *prometheus.CounterVec
	// AuthzAdminCalls counts provisioning calls into authorization-svc's admin
	// API by operation and outcome.
	//
	// This is the series that matters most here. This service is a front door
	// in front of that API; when the propagation fails, the register refuses
	// the write and stays truthful — but a SUSTAINED failure means nobody can
	// author or retire a role at all, and every symptom of it is a 503 that
	// looks like any other.
	AuthzAdminCalls *prometheus.CounterVec
	// OutboxPending is the depth of the unpublished event backlog.
	OutboxPending prometheus.Gauge
	// OutboxPublished counts events the relay handed to Kafka.
	OutboxPublished *prometheus.CounterVec
	// OutboxFailures counts relay drain attempts that failed.
	OutboxFailures prometheus.Counter
	// OutboxOldestAgeSeconds is the age of the oldest unpublished event. Depth
	// alone cannot distinguish a busy moment from a stalled relay: a backlog of
	// ten that is three seconds old is healthy, and a backlog of ten that is an
	// hour old means a role retirement has not reached the consumer that ends
	// the sessions still carrying its grants.
	OutboxOldestAgeSeconds prometheus.Gauge
}

// Role write outcomes. Every one is a label value that must exist from startup
// — see NewDomainWith.
const (
	WriteCreated             = "created"
	WriteUpdated             = "updated"
	WriteReplayed            = "replayed"
	WriteInvalidRequest      = "invalid_request"
	WriteForbidden           = "forbidden"
	WriteNotFound            = "not_found"
	WriteConflict            = "conflict"
	WriteAuthzUnavailable    = "authz_unavailable"
	WriteAuthzAdminUnavail   = "authz_admin_unavailable"
	WriteStoreUnavailable    = "store_unavailable"
	WriteIdentityMissing     = "identity_missing"
	WriteNoChange            = "no_change"
	WriteDetached            = "detached"
	WriteAlreadyDetachedNoop = "already_detached"
)

// AuthZ outcomes.
const (
	AuthZGranted     = "granted"
	AuthZDenied      = "denied"
	AuthZUnavailable = "unavailable"
)

// Admin API operations.
const (
	AdminCreateRole   = "create_role"
	AdminSetRoleState = "set_role_active"
	AdminCreateBundle = "create_bundle"
	AdminSetBundle    = "set_bundle_active"
)

// Admin API outcomes.
const (
	AdminOK          = "ok"
	AdminUnavailable = "unavailable"
)

// NewDomain registers this service's decision counters and pre-creates every
// label combination at zero.
//
// The pre-creation is the point, not housekeeping. A Prometheus series that has
// never been observed and a series reading zero are indistinguishable to an
// alert expression: `increase(...{outcome="forbidden"}[15m]) > 0` evaluates
// against no data at all until the first refusal, so a rule written to catch
// the first one stays silent through exactly the event it exists for.
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
		RoleWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "access_control_role_writes_total",
			Help:        "Role-definition write attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		BundleWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "access_control_bundle_writes_total",
			Help:        "Permission-bundle write attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		AuthZDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "access_control_authz_decisions_total",
			Help:        "Calls to authorization-svc /v1/authorize by action and outcome.",
			ConstLabels: labels,
		}, []string{"action", "outcome"}),
		AuthzAdminCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "access_control_authz_admin_calls_total",
			Help:        "Provisioning calls into authorization-svc's admin API by operation and outcome.",
			ConstLabels: labels,
		}, []string{"operation", "outcome"}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "access_control_outbox_pending",
			Help:        "Unpublished events in the transactional outbox.",
			ConstLabels: labels,
		}),
		OutboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "access_control_outbox_published_total",
			Help:        "Events handed to Kafka by the relay, by event type.",
			ConstLabels: labels,
		}, []string{"event_type"}),
		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "access_control_outbox_failures_total",
			Help:        "Relay drain attempts that failed.",
			ConstLabels: labels,
		}),
		OutboxOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "access_control_outbox_oldest_age_seconds",
			Help:        "Age of the oldest unpublished event in the outbox.",
			ConstLabels: labels,
		}),
	}

	reg.MustRegister(
		d.RoleWrites, d.BundleWrites, d.AuthZDecisions, d.AuthzAdminCalls,
		d.OutboxPending, d.OutboxPublished, d.OutboxFailures, d.OutboxOldestAgeSeconds,
	)

	writeOutcomes := []string{
		WriteCreated, WriteUpdated, WriteReplayed, WriteInvalidRequest, WriteForbidden,
		WriteNotFound, WriteConflict, WriteAuthzUnavailable, WriteAuthzAdminUnavail,
		WriteStoreUnavailable, WriteIdentityMissing, WriteNoChange, WriteDetached,
		WriteAlreadyDetachedNoop,
	}
	for _, o := range writeOutcomes {
		d.RoleWrites.WithLabelValues(o)
		d.BundleWrites.WithLabelValues(o)
	}
	for _, action := range []string{ActionRoleManage} {
		for _, o := range []string{AuthZGranted, AuthZDenied, AuthZUnavailable} {
			d.AuthZDecisions.WithLabelValues(action, o)
		}
	}
	for _, op := range []string{AdminCreateRole, AdminSetRoleState, AdminCreateBundle, AdminSetBundle} {
		for _, o := range []string{AdminOK, AdminUnavailable} {
			d.AuthzAdminCalls.WithLabelValues(op, o)
		}
	}
	for _, t := range []string{"role.created", "role.updated", "permission.bundle.updated"} {
		d.OutboxPublished.WithLabelValues(t)
	}

	return d
}

// ActionRoleManage is the authorization action this service checks on every
// write. It lives here as well as in the handler so the pre-created series and
// the checks cannot drift apart.
//
// ── WHY IT IS "ROLE_MANAGE" ─────────────────────────────────────────────────
//
// The handler asked for "ACCESS_ROLE_MANAGE" and nothing else in the estate has
// ever used that name. The seed that provisions the demo grants
// (deployments/scripts/seed-demo-rbac.ps1) attaches ACCESS_CONTROL_FULL with
// permitted_actions ["ROLE_MANAGE"]; the console's own copy tells the operator
// a 403 means "you hold no ROLE_MANAGE grant"; the live bundle in
// authorization-svc says ROLE_MANAGE. The service was the only party asking for
// the other name, so every write it served was refused 403 "authorization
// denied" — a refusal indistinguishable from a genuinely ungranted operator,
// on a page whose reads all worked.
const ActionRoleManage = "ROLE_MANAGE"
