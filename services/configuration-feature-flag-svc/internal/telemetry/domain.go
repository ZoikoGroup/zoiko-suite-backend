package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Domain holds the counters that describe what this service DECIDED, as
// distinct from the HTTP counters in telemetry.go which describe what it
// answered.
//
// The distinction is the point, and on this service it is sharper than most.
// Every interesting failure here produces a perfectly ordinary-looking
// response:
//
//   - A write refused because the caller holds no CONFIGURATION_WRITE grant is
//     a 403 like any other.
//   - A write refused because authorization-svc could not be reached is a 503
//     that reads like a dependency blip.
//   - An idempotent no-op — the value was already what was submitted — is a
//     200 indistinguishable from a successful read.
//
// http_requests_total{status_code="403"} cannot tell a genuinely ungranted
// operator from this service asking for an action name nobody provisions, and
// {status_code="200"} cannot tell a recorded change from a change that was
// silently declined because it changed nothing. Those are the two questions an
// operator actually asks about a configuration service, and neither is
// answerable from the HTTP series alone.
type Domain struct {
	// ConfigWrites counts every POST /v1/config attempt by outcome.
	ConfigWrites *prometheus.CounterVec
	// FlagWrites counts every POST /v1/flags attempt by outcome.
	FlagWrites *prometheus.CounterVec

	// AuthZDecisions counts calls to authorization-svc /v1/authorize by action
	// and outcome. "unavailable" is kept apart from "denied" because the two
	// need opposite responses: one is a permissions problem, the other an
	// outage.
	AuthZDecisions *prometheus.CounterVec

	// GlobalScopeWrites counts writes that targeted the environment-wide
	// default rather than one tenant.
	//
	// Its own series because of what a global write IS: a value that takes
	// effect for every tenant in the environment that has not set its own. It
	// is the single highest-blast-radius operation this service offers, and in
	// the HTTP series it is a 201 identical to a write affecting one tenant.
	GlobalScopeWrites *prometheus.CounterVec

	// GovernedWrites counts the AA-001 governed write tiers — definition
	// registration/publish, override activation, change and emergency change
	// lifecycle, release-plan publication and attestation — by action and
	// outcome. Kept apart from ConfigWrites/FlagWrites because those two
	// describe POST /v1/config and POST /v1/flags specifically, and a governed
	// write is a different act: it is gated on a definition and lands in
	// snapshot history with its own event family.
	GovernedWrites *prometheus.CounterVec

	// OutboxPending is the depth of the unpublished event backlog.
	OutboxPending prometheus.Gauge
	// OutboxPublished counts events the relay handed to Kafka, by type.
	OutboxPublished *prometheus.CounterVec
	// OutboxFailures counts relay drain attempts that failed.
	OutboxFailures prometheus.Counter
	// OutboxOldestAgeSeconds is the age of the oldest unpublished event.
	//
	// Depth alone cannot distinguish a busy moment from a stalled relay: a
	// backlog of ten that is three seconds old is healthy, and a backlog of ten
	// that is an hour old means consumers have been acting on a superseded
	// configuration value for an hour while this service displays the new one.
	OutboxOldestAgeSeconds prometheus.Gauge
}

// Write outcomes. Every one is a label value that must exist from startup —
// see NewDomainWith.
const (
	// WriteCreated is a real transition: a first value for this scope, or a
	// genuinely different one.
	WriteCreated = "created"
	// WriteNoChange is the idempotent path: the submitted value already
	// equalled the effective one, so nothing was written. Counted separately
	// because it is a 200 and a success, and a service that suddenly answers
	// only this has stopped recording changes.
	WriteNoChange = "no_change"

	WriteInvalidRequest   = "invalid_request"
	WriteTooLarge         = "too_large"
	WriteForbidden        = "forbidden"
	WriteConflict         = "conflict"
	WriteAuthzUnavailable = "authz_unavailable"
	WriteStoreUnavailable = "store_unavailable"
	WriteIdentityMissing  = "identity_missing"
	WriteTenantMismatch   = "tenant_mismatch"
)

// AuthZ outcomes.
const (
	AuthZGranted     = "granted"
	AuthZDenied      = "denied"
	AuthZUnavailable = "unavailable"
)

// The authorization actions this service checks. They live here as well as in
// the handler so the pre-created series and the checks cannot drift apart — a
// series that exists but is never incremented is a visible zero, whereas an
// action the handler asks for and this list omits is invisible entirely.
//
// ── WHY THERE ARE FOUR AND NOT TWO ──────────────────────────────────────────
//
// A config entry or flag written with no tenant_id is the environment-wide
// DEFAULT: it applies to every tenant that has not set its own value. Writing
// one and writing your own organisation value are not the same act and must
// not be the same grant — but they used to be. Both paths authorized
// CONFIGURATION_WRITE (or FEATURE_FLAG_WRITE) against the platform scope, so a
// principal provisioned to manage one organisation settings could also change
// the default every OTHER organisation reads, and the refusal that should have
// stopped it never ran. RLS cannot catch this either: migration 000002 WITH
// CHECK admits tenant_id IS NULL unconditionally, because a global row
// genuinely belongs to no tenant.
//
// The two _GLOBAL_ actions are seeded in deployments/scripts/seed-demo-rbac.ps1
// under CONFIG_FULL alongside the originals. That is not a detail: an action
// name nothing in the estate grants makes every write 403 while every read
// works, which reads as an under-granted operator rather than as a service
// asking for a name nobody defines.
const (
	ActionConfigWrite       = "CONFIGURATION_WRITE"
	ActionConfigGlobalWrite = "CONFIGURATION_GLOBAL_WRITE"
	ActionFlagWrite         = "FEATURE_FLAG_WRITE"
	ActionFlagGlobalWrite   = "FEATURE_FLAG_GLOBAL_WRITE"
)

// Event types, for the pre-created outbox series. All ten must stay in step
// with internal/events/publisher.go and asyncapi.yaml — the outbox CHECK in
// migration 000008 admits exactly these strings, so a drift here fails the
// next write, loudly, at the database.
const (
	EventConfigUpdated = "config.updated"
	EventFlagUpdated   = "feature_flag.updated"

	EventSnapshotPublished   = "config.snapshot.published"
	EventVersionPublished    = "config.version.published"
	EventOverrideActivated   = "config.override.activated"
	EventReleaseActivated    = "flag.release.activated"
	EventKillSwitchActivated = "flag.kill_switch.activated"
	EventChangeVerified      = "config.change.verified"
	EventDriftDetected       = "config.drift.detected"
	EventEmergencyExpired    = "config.emergency.expired"
)

// allEventTypes is the single list both the pre-created series loop and the
// telemetry tests read, so a new event cannot be added in one place only.
var allEventTypes = []string{
	EventConfigUpdated, EventFlagUpdated,
	EventSnapshotPublished, EventVersionPublished, EventOverrideActivated,
	EventReleaseActivated, EventKillSwitchActivated, EventChangeVerified,
	EventDriftDetected, EventEmergencyExpired,
}

// NewDomain registers this service decision counters and pre-creates every
// label combination at zero.
//
// The pre-creation is the point, not housekeeping. A Prometheus series that has
// never been observed and a series reading zero are indistinguishable to an
// alert expression: increase(...{outcome="forbidden"}[15m]) > 0 evaluates
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
		ConfigWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_config_writes_total",
			Help:        "POST /v1/config attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		FlagWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_flag_writes_total",
			Help:        "POST /v1/flags attempts by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		AuthZDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_authz_decisions_total",
			Help:        "Calls to authorization-svc /v1/authorize by action and outcome.",
			ConstLabels: labels,
		}, []string{"action", "outcome"}),
		GlobalScopeWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_global_scope_writes_total",
			Help:        "Writes targeting the environment-wide default rather than one tenant.",
			ConstLabels: labels,
		}, []string{"resource", "outcome"}),
		GovernedWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_governed_writes_total",
			Help:        "AA-001 governed writes by action and outcome.",
			ConstLabels: labels,
		}, []string{"action", "outcome"}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "configuration_outbox_pending",
			Help:        "Unpublished events in the transactional outbox.",
			ConstLabels: labels,
		}),
		OutboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "configuration_outbox_published_total",
			Help:        "Events handed to Kafka by the relay, by event type.",
			ConstLabels: labels,
		}, []string{"event_type"}),
		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "configuration_outbox_failures_total",
			Help:        "Relay drain attempts that failed.",
			ConstLabels: labels,
		}),
		OutboxOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "configuration_outbox_oldest_age_seconds",
			Help:        "Age of the oldest unpublished event in the outbox.",
			ConstLabels: labels,
		}),
	}

	reg.MustRegister(
		d.ConfigWrites, d.FlagWrites, d.AuthZDecisions, d.GlobalScopeWrites,
		d.GovernedWrites,
		d.OutboxPending, d.OutboxPublished, d.OutboxFailures, d.OutboxOldestAgeSeconds,
	)

	for _, o := range []string{
		WriteCreated, WriteNoChange, WriteInvalidRequest, WriteTooLarge,
		WriteForbidden, WriteConflict, WriteAuthzUnavailable,
		WriteStoreUnavailable, WriteIdentityMissing, WriteTenantMismatch,
	} {
		d.ConfigWrites.WithLabelValues(o)
		d.FlagWrites.WithLabelValues(o)
	}
	for _, a := range []string{
		ActionConfigWrite, ActionConfigGlobalWrite, ActionFlagWrite, ActionFlagGlobalWrite,
	} {
		for _, o := range []string{AuthZGranted, AuthZDenied, AuthZUnavailable} {
			d.AuthZDecisions.WithLabelValues(a, o)
		}
	}
	for _, res := range []string{"config", "flag"} {
		for _, o := range []string{WriteCreated, WriteNoChange, WriteForbidden} {
			d.GlobalScopeWrites.WithLabelValues(res, o)
		}
	}
	for _, a := range []string{
		ActionConfigWrite, ActionConfigGlobalWrite, ActionFlagWrite, ActionFlagGlobalWrite,
	} {
		for _, o := range []string{
			WriteCreated, WriteNoChange, WriteInvalidRequest, WriteTooLarge,
			WriteForbidden, WriteConflict, WriteAuthzUnavailable,
			WriteStoreUnavailable, WriteIdentityMissing, WriteTenantMismatch,
		} {
			d.GovernedWrites.WithLabelValues(a, o)
		}
	}
	for _, t := range allEventTypes {
		d.OutboxPublished.WithLabelValues(t)
	}

	return d
}
