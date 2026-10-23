// Package inbound is XIC-03, the Inbound Exchange & Canonicalization Gateway of ZS-SVC-T-001 (§8).
//
// Every external delivery passes the same ordered pipeline before any domain sees it: size and rate envelope,
// resolution of the connector operation and candidate connection from the trusted route, authentication of the
// sender, tenant resolution from the connection (never from the payload), replay-window and duplicate detection,
// media-type and exact-schema validation, data-policy screening, deterministic mapping into the canonical contract,
// and a durable handoff to the owning domain. Any uncertain or unsupported step rejects or quarantines; nothing is
// guessed. Transport acceptance (this service durably holds the exchange) and business acceptance (the domain
// said yes) are recorded separately and never merged into one status.
package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// Exchange states (§4).
const (
	StateReceived      = "RECEIVED"
	StateAuthenticated = "AUTHENTICATED"
	StateDuplicate     = "DUPLICATE"
	StateValidated     = "VALIDATED"
	StateQuarantined   = "QUARANTINED"
	StateHandedOff     = "HANDED_OFF"
	StateRejected      = "REJECTED"
)

// Domain outcomes: business acceptance, separate from transport (I-12).
const (
	DomainNA       = "NOT_APPLICABLE"
	DomainPending  = "PENDING"
	DomainAccepted = "ACCEPTED"
	DomainRejected = "REJECTED"
	DomainUnknown  = "UNKNOWN"
)

// Quarantine classes (§8.3). IDEMPOTENCY is the class for a changed body under a reused key (NP-07).
const (
	ClassSchema             = "SCHEMA"
	ClassMapping            = "MAPPING"
	ClassAuthContext        = "AUTH_CONTEXT"
	ClassDataPolicy         = "DATA_POLICY"
	ClassDomainPrecondition = "DOMAIN_PRECONDITION"
	ClassIdempotency        = "IDEMPOTENCY"
)

// Handoff target states.
const (
	TargetDraft     = "DRAFT"
	TargetActive    = "ACTIVE"
	TargetSuspended = "SUSPENDED"
	TargetRetired   = "RETIRED"
)

// Quarantine request actions and states.
const (
	ActionRelease = "RELEASE"
	ActionDismiss = "DISMISS"
	ReqPending    = "PENDING"
	ReqApproved   = "APPROVED"
	ReqRejected   = "REJECTED"
	ReqUsed       = "USED"
)

// Errors.
var (
	ErrNotFound        = errors.New("exchange not found")
	ErrTargetNotFound  = errors.New("handoff target not found")
	ErrTargetExists    = errors.New("a handoff target already exists for this canonical contract")
	ErrDedupe          = errors.New("duplicate dedupe key")
	ErrVersionConflict = errors.New("version conflict: the object changed since it was read")
	ErrTransition      = errors.New("transition is not allowed from the current state")
	ErrSelfAction      = errors.New("that step must be taken by someone other than the person who prepared it")
	ErrNotPlatform     = errors.New("only the platform scope may do that")
	ErrBadRequest      = errors.New("bad request")
	ErrNotReleasable   = errors.New("this quarantine cannot be released")
	ErrNoRaw           = errors.New("the raw payload is not retained, so the exchange cannot be replayed from here; ask the sender to redeliver")
	ErrRequestNotFound = errors.New("request not found")
	ErrRequestState    = errors.New("request is not in a state that allows this")
	ErrEgress          = errors.New("the target is not an allowed egress destination")
	ErrDependency      = errors.New("a dependency needed for this is unavailable")
)

// Exchange is the durable record of one delivery. Bodies are internal: the API never returns them.
type Exchange struct {
	ExchangeID         string          `json:"exchange_id"`
	TenantID           string          `json:"tenant_id"`
	ConnectionID       string          `json:"connection_id"`
	ConnectorID        string          `json:"connector_id"`
	OperationID        string          `json:"operation_id"`
	ContractVersionID  string          `json:"contract_version_id"`
	State              string          `json:"state"`
	DomainOutcome      string          `json:"domain_outcome"`
	QuarantineClass    string          `json:"quarantine_class,omitempty"`
	ReasonCode         string          `json:"reason_code,omitempty"`
	XICCode            string          `json:"xic_code,omitempty"`
	OriginalExchangeID string          `json:"original_exchange_id,omitempty"`
	DedupeKey          string          `json:"-"`
	SourceRefHash      string          `json:"source_ref_hash,omitempty"`
	BodyHash           string          `json:"body_hash"`
	BodySize           int             `json:"body_size"`
	ContentType        string          `json:"content_type,omitempty"`
	SourceSchema       string          `json:"source_schema,omitempty"`
	MappingVersionID   string          `json:"mapping_version_id,omitempty"`
	CanonicalContract  string          `json:"canonical_contract,omitempty"`
	CanonicalHash      string          `json:"canonical_hash,omitempty"`
	Lineage            json.RawMessage `json:"lineage,omitempty"`
	Raw                []byte          `json:"-"`
	Canonical          []byte          `json:"-"`
	RawExpiresAt       *time.Time      `json:"raw_expires_at,omitempty"`
	RawRetained        bool            `json:"raw_retained"`
	TargetID           string          `json:"target_id,omitempty"`
	HandoffAttempts    int             `json:"handoff_attempts"`
	NextAttemptAt      *time.Time      `json:"next_attempt_at,omitempty"`
	LastErrorCode      string          `json:"last_error_code,omitempty"`
	DomainRef          string          `json:"domain_ref,omitempty"`
	DomainStatus       int             `json:"domain_status,omitempty"`
	AuthMechanism      string          `json:"auth_mechanism,omitempty"`
	SourceNetHash      string          `json:"source_net_hash,omitempty"`
	CorrelationID      string          `json:"correlation_id,omitempty"`
	AttemptNo          int             `json:"attempt_no"`
	Version            int64           `json:"version"`
	ReceivedAt         time.Time       `json:"received_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// Target is a platform handoff target: where the canonical payloads of one canonical contract are delivered.
type Target struct {
	TargetID          string `json:"target_id"`
	CanonicalContract string `json:"canonical_contract"`
	BaseURL           string `json:"base_url"`
	Path              string `json:"path"`
	Region            string `json:"region"`
	// LegalEntityPointer says where in the canonical payload the legal entity id lives, so the handoff can send the
	// canonical input contract the domain enforces. Empty means the domain takes none.
	LegalEntityPointer string     `json:"legal_entity_pointer,omitempty"`
	Status             string     `json:"status"`
	Fingerprint        string     `json:"fingerprint"`
	CreatedBy          string     `json:"created_by"`
	ApprovedBy         string     `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	Version            int64      `json:"version"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// Event is the append-only evidence of an exchange change. It never carries payload values.
type Event struct {
	Kind   string          `json:"kind"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	Actor  string          `json:"actor"`
	At     time.Time       `json:"at"`
}

// QRequest is a quarantine release or dismissal request, decided by someone else.
type QRequest struct {
	RequestID   string     `json:"request_id"`
	ExchangeID  string     `json:"exchange_id"`
	Action      string     `json:"action"`
	Reason      string     `json:"reason"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
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
	State        string
	ConnectionID string
	Class        string
	Limit        int
}

// Tx is what Mutate hands to its function.
type Tx struct {
	E        *Exchange
	Requests []QRequest

	NewEvent      *Event
	NewRequest    *QRequest
	UpdateRequest *QRequest
	Out           []OutEvent
}

// Repo is the persistence boundary of XIC-03.
type Repo interface {
	// Insert stores a new exchange in its final pipeline state with its event and outbox events, atomically. When the
	// exchange claims a dedupe key already held it returns ErrDedupe and the holder in existing, and writes nothing.
	Insert(ctx context.Context, e *Exchange, ev Event, out []OutEvent) (existing *Exchange, err error)
	// InsertDuplicate stores a terminal DUPLICATE row pointing at the original.
	InsertDuplicate(ctx context.Context, e *Exchange, ev Event, out []OutEvent) error
	Get(ctx context.Context, id string) (*Exchange, error)
	// GetByDedupe finds the exchange holding a dedupe key.
	GetByDedupe(ctx context.Context, tenant, key string) (*Exchange, error)
	List(ctx context.Context, f Filter) ([]Exchange, error)
	Events(ctx context.Context, id string) ([]Event, error)
	Requests(ctx context.Context, id string) ([]QRequest, error)
	Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Exchange, error)

	// Due returns VALIDATED exchanges whose handoff is due, across tenants (read only).
	Due(ctx context.Context, now time.Time, limit int) ([]DueRef, error)
	// PurgeExpired removes raw and canonical bodies whose retention has passed.
	PurgeExpired(ctx context.Context, now time.Time, limit int) (int, error)

	CreateTarget(ctx context.Context, t *Target, ev OutEvent) error
	GetTarget(ctx context.Context, id string) (*Target, error)
	ActiveTarget(ctx context.Context, canonicalContract string) (*Target, error)
	ListTargets(ctx context.Context) ([]Target, error)
	MutateTarget(ctx context.Context, id string, fn func(t *Target) (*OutEvent, error)) (*Target, error)
	// RecordSignal writes a security-signal event in its own transaction.
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error

	// Summaries reads what became of a set of exchanges (state and domain outcome), for the batch engine that
	// reports on rows. An id that is not the caller's tenant's is simply absent.
	Summaries(ctx context.Context, ids []string) (map[string]ExchangeSummary, error)
	// Held lists the QUARANTINED exchanges of one connection and operation received within [from, to], oldest first.
	Held(ctx context.Context, connectionID, operationID string, from, to time.Time, limit int) ([]string, error)
}

// DueRef names an exchange the handoff worker should drive.
type DueRef struct{ TenantID, ExchangeID string }

// Contracts is the read side of the registry.
type Contracts interface {
	GetVersion(ctx context.Context, id string) (*registry.ContractVersion, error)
}

func withTenant(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}

// EventTypes lists every event XIC-03 can emit (ZS-SVC-T-001 section 14.1 integration.inbound.* plus the delivery,
// release and security events around them). A test proves each is produced by a real flow and is a valid envelope.
var EventTypes = []string{
	"integration.inbound.accepted", "integration.inbound.quarantined", "integration.inbound.rejected", "integration.inbound.duplicate",
	"integration.inbound.handed-off", "integration.inbound.handoff-unknown", "integration.inbound.handoff-stalled",
	"integration.inbound.quarantine-released", "integration.inbound.quarantine-dismissed",
	"integration.handoff-target.created", "integration.handoff-target.activated", "integration.handoff-target.suspended",
	"integration.security.inbound-tenant-claim-mismatch", "integration.security.inbound-account-claim-mismatch",
	"integration.security.handoff-egress-blocked", "integration.security.handoff-region-blocked",
}

// ExchangeSummary is the part of an exchange a batch needs to report on a row.
type ExchangeSummary struct {
	State           string
	DomainOutcome   string
	QuarantineClass string
	ReasonCode      string
	CanonicalHash   string
}
