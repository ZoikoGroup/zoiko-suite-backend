// Package outbound is XIC-04, Outbound Delivery Control of ZS-SVC-T-001 (§9): durable outbound attempts, safe
// submit and resume, provider idempotency and UNKNOWN resolution.
//
// The whole package serves one rule: a logical side effect at an external provider is reserved durably BEFORE any
// network call, and when the answer is lost or ambiguous the original attempt is resolved (provider reference,
// status query, callback or reconciliation) - it is never blindly repeated. A second submission of the same
// logical attempt happens only where the contract declared the provider's idempotency key makes that safe, and
// then it carries the same key. Destinations come from the connection's governed endpoint profile, never from the
// caller; the destination is checked against egress policy (DNS, address ranges, region) before any credential is
// attached; redirects are not followed, so a credential is never forwarded to another host.
package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// Attempt states (§9.2).
const (
	StateReserved   = "RESERVED"
	StateSubmitting = "SUBMITTING"
	StatePending    = "PENDING"
	StateUnknown    = "UNKNOWN"
	StateSettled    = "SETTLED"
	StateFailed     = "FAILED"
	StateCancelled  = "CANCELLED"
)

// Resolution methods: how an attempt reached its final transport state.
const (
	MethodSubmit         = "SUBMIT"
	MethodStatusQuery    = "STATUS_QUERY"
	MethodCallback       = "CALLBACK"
	MethodReconciliation = "RECONCILIATION"
	MethodManual         = "MANUAL"
)

// Ambiguity and failure reasons. They are stable machine values and never carry a payload value.
const (
	ReasonTimeout            = "TIMEOUT_AFTER_SEND"
	ReasonConnectionLost     = "CONNECTION_LOST_AFTER_SEND"
	ReasonProviderUnavail    = "PROVIDER_UNAVAILABLE"
	ReasonRateLimited        = "PROVIDER_RATE_LIMITED"
	ReasonCrashRecovery      = "CRASH_DURING_SUBMIT"
	ReasonUnclassified       = "UNCLASSIFIED_RESPONSE"
	ReasonStatusUnrecognised = "STATUS_NOT_RECOGNISED"
	ReasonProviderRejected   = "PROVIDER_REJECTED"
	ReasonAuthRejected       = "PROVIDER_AUTH_REJECTED"
	ReasonRedirectRefused    = "REDIRECT_REFUSED"
	ReasonBlocked            = "BLOCKED_BEFORE_SEND"
	ReasonOperatorCancelled  = "OPERATOR_CANCELLED"
	ReasonStatusNotFound     = "STATUS_NOT_FOUND"
)

// Errors.
var (
	ErrNotFound              = errors.New("outbound attempt not found")
	ErrVersionConflict       = errors.New("version conflict: the attempt changed since it was read")
	ErrTransition            = errors.New("transition is not allowed from the current state")
	ErrBadRequest            = errors.New("bad request")
	ErrPayloadConflict       = errors.New("this domain request already has an attempt with a different payload")
	ErrUnknownMustBeResolved = errors.New("the attempt is UNKNOWN or PENDING: resolve the original attempt; a second submission is not authorized for this operation")
	ErrBodyNotHeld           = errors.New("the request body is not stored for this attempt: send it again with the submit request")
	ErrBodyMismatch          = errors.New("the supplied body does not match the payload hash of the attempt")
	ErrSelfAction            = errors.New("a manual resolution must be made by someone other than whoever reserved the attempt")
	ErrNotResolvable         = errors.New("the contract defines no way to resolve this operation")
	ErrAuthUnsupported       = errors.New("the operation's authentication mechanism is not supported for outbound calls")
	ErrOperationDirection    = errors.New("the operation is not an outbound operation")
	ErrBlocked               = errors.New("the destination or credential check refused the call before anything was sent")
	ErrDependency            = errors.New("a dependency needed for this is unavailable")
)

// Attempt is the durable record of one logical outbound operation. The request body is internal: the API never
// returns it.
type Attempt struct {
	AttemptID           string          `json:"attempt_id"`
	TenantID            string          `json:"tenant_id"`
	ConnectionID        string          `json:"connection_id"`
	ConnectorID         string          `json:"connector_id"`
	OperationID         string          `json:"operation_id"`
	ContractVersionID   string          `json:"contract_version_id"`
	DomainRequestRef    string          `json:"domain_request_ref"`
	PayloadProfile      string          `json:"payload_profile,omitempty"`
	PayloadHash         string          `json:"payload_hash"`
	PayloadSize         int             `json:"payload_size"`
	ProviderIdemRefHash string          `json:"provider_idempotency_ref_hash,omitempty"`
	ResubmitAllowed     bool            `json:"resubmit_allowed"`
	State               string          `json:"state"`
	AmbiguityReason     string          `json:"ambiguity_reason,omitempty"`
	ProviderReference   string          `json:"provider_reference,omitempty"`
	ProviderStatus      int             `json:"provider_status,omitempty"`
	SubmitCount         int             `json:"submit_count"`
	LastSubmitAt        *time.Time      `json:"last_submit_at,omitempty"`
	ResolveCount        int             `json:"resolve_count"`
	NextResolutionAt    *time.Time      `json:"next_resolution_at,omitempty"`
	EscalatedAt         *time.Time      `json:"escalated_at,omitempty"`
	ResolutionMethod    string          `json:"resolution_method,omitempty"`
	ResolvedBy          string          `json:"resolved_by,omitempty"`
	EvidenceRef         string          `json:"evidence_ref,omitempty"`
	FinalOutcomeRef     json.RawMessage `json:"final_outcome_ref,omitempty"`
	Body                []byte          `json:"-"`
	BodyExpiresAt       *time.Time      `json:"body_expires_at,omitempty"`
	BodyRetained        bool            `json:"body_retained"`
	CorrelationID       string          `json:"correlation_id,omitempty"`
	RequestedBy         string          `json:"requested_by"`
	Version             int64           `json:"version"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// Open reports whether the attempt can still change.
func (a *Attempt) Open() bool {
	switch a.State {
	case StateSettled, StateFailed, StateCancelled:
		return false
	}
	return true
}

// Event is the append-only evidence of an attempt change. It never carries payload values or credentials.
type Event struct {
	Kind   string          `json:"kind"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	Actor  string          `json:"actor"`
	At     time.Time       `json:"at"`
}

// OutEvent is a domain event for the transactional outbox.
type OutEvent struct {
	Type          string
	AggregateType string
	AggregateID   string
	ActorID       string
	CorrelationID string
	Data          map[string]any
}

// Actor is who is acting.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

// Filter narrows a list.
type Filter struct {
	State            string
	ConnectionID     string
	DomainRequestRef string
	Limit            int
}

// Tx is what Mutate hands to its function.
type Tx struct {
	A     *Attempt
	Event *Event
	Out   []OutEvent
}

// Repo is the persistence boundary of XIC-04.
type Repo interface {
	// Reserve stores a new attempt in RESERVED with its event and outbox events, atomically. When the logical
	// operation already has an attempt it writes nothing and returns that attempt with ErrDuplicate.
	Reserve(ctx context.Context, a *Attempt, ev Event, out []OutEvent) (existing *Attempt, err error)
	Get(ctx context.Context, id string) (*Attempt, error)
	List(ctx context.Context, f Filter) ([]Attempt, error)
	Events(ctx context.Context, id string) ([]Event, error)
	Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Attempt, error)
	// DueForResolution returns attempts whose resolution is due (UNKNOWN, PENDING, or SUBMITTING past its lease) or that
	// have been open since before escalateBefore without having been escalated,
	// across tenants (read only).
	DueForResolution(ctx context.Context, now, escalateBefore time.Time, limit int) ([]DueRef, error)
	// PurgeBodies removes stored bodies that are no longer needed: final attempts, and open ones past retention.
	PurgeBodies(ctx context.Context, now time.Time, limit int) (int, error)
	// RecordSignal writes a security-signal event in its own transaction.
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error
}

// ErrDuplicate is returned by Reserve when the logical operation already has an attempt.
var ErrDuplicate = errors.New("attempt already reserved for this logical operation")

// DueRef names an attempt the resolver should drive.
type DueRef struct{ TenantID, AttemptID string }

func withTenant(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}

// EventTypes lists every event XIC-04 can emit (ZS-SVC-T-001 section 14.1 integration.outbound.* plus the
// reservation, cancellation, escalation and security events around them). A test proves each is produced by a real
// flow and is a valid envelope.
var EventTypes = []string{
	"integration.outbound.reserved", "integration.outbound.submitted", "integration.outbound.pending", "integration.outbound.unknown",
	"integration.outbound.resolved", "integration.outbound.cancelled", "integration.outbound.blocked",
	"integration.outbound.unknown-escalated",
	"integration.security.outbound-egress-blocked", "integration.security.outbound-region-blocked", "integration.security.outbound-redirect-refused",
}

func tenantFromCtx(ctx context.Context) string { return middleware.GetTenantID(ctx) }
