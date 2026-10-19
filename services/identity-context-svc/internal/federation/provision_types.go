package federation

import (
	"context"
	"errors"
	"time"
)

// Provisioning event statuses and sources.
const (
	ProvReceived   = "RECEIVED"
	ProvValidated  = "VALIDATED"
	ProvApplied    = "APPLIED"
	ProvRejected   = "REJECTED"
	ProvReconciled = "RECONCILED"

	OpCreate      = "CREATE"
	OpUpdate      = "UPDATE"
	OpDeprovision = "DEPROVISION"
	OpReconcile   = "RECONCILE"

	SrcSCIM      = "SCIM"
	SrcJIT       = "JIT"
	SrcReconcile = "RECONCILE"
)

// Assignment statuses (the work queue of what federation asked access control for).
const (
	AsgPlanned       = "PLANNED"
	AsgProvisioned   = "PROVISIONED"
	AsgRefused       = "REFUSED"
	AsgRevokePending = "REVOKE_PENDING"
	AsgRevoked       = "REVOKED"
)

// Sync exception kinds.
const (
	ExcPrivilegedRefused   = "PRIVILEGED_REFUSED"
	ExcRequestRefused      = "REQUEST_REFUSED"
	ExcAccessControlDown   = "ACCESS_CONTROL_UNAVAILABLE"
	ExcAmbiguousIdentity   = "AMBIGUOUS_IDENTITY"
	ExcAmbiguousMapping    = "AMBIGUOUS_MAPPING"
	ExcEventRejected       = "EVENT_REJECTED"
	ExcDeprovisionLate     = "DEPROVISION_LATE"
	ExcMissingInPlatform   = "MISSING_IN_PLATFORM"
	ExcSessionEvictionFail = "SESSION_EVICTION_FAILED"
)

// Errors of the provisioning pipeline.
var (
	// ErrAmbiguousIdentity: the directory record would share an email address with a different person already
	// here. An address is never an identity binding, so it is quarantined for a human to resolve, not merged.
	ErrAmbiguousIdentity = errors.New("another principal in this tenant already uses that email address")
	// ErrStaleLink: the record changed between reading it and applying a change computed from it.
	ErrStaleLink = errors.New("the directory record changed concurrently")
	// ErrDeprovisionInProgress: reactivation waits until the earlier deprovisioning has finished.
	ErrDeprovisionInProgress = errors.New("the earlier deprovisioning is still in progress")
	// ErrDedupeMismatch: an event identity was reused with a different payload.
	ErrDedupeMismatch = errors.New("the event identity was already used with a different payload")
	ErrUserNotFound   = errors.New("directory user not found")
	ErrTokenInvalid   = errors.New("the SCIM token is not valid")
	ErrMappingState   = errors.New("the mapping version is not awaiting a decision")
	ErrExceptionState = errors.New("the exception is not open")
)

// ProvEvent is one entry of the provisioning ledger.
type ProvEvent struct {
	EventID      string         `json:"event_id"`
	TenantID     string         `json:"tenant_id"`
	ConnectionID string         `json:"connection_id"`
	DedupeKey    string         `json:"dedupe_key"`
	Source       string         `json:"source"`
	Operation    string         `json:"operation"`
	Subject      string         `json:"subject,omitempty"`
	PayloadHash  string         `json:"payload_hash"`
	Status       string         `json:"status"`
	Reason       string         `json:"reason,omitempty"`
	Result       map[string]any `json:"result,omitempty"`
	ReceivedAt   time.Time      `json:"received_at"`
	DecidedAt    *time.Time     `json:"decided_at,omitempty"`
}

// Assignment is one assignment federation asked of access control, and what is still owed on it.
type Assignment struct {
	AssignmentID     string    `json:"assignment_id"`
	TenantID         string    `json:"tenant_id"`
	ConnectionID     string    `json:"connection_id"`
	LinkID           string    `json:"link_id"`
	PrincipalID      string    `json:"principal_id"`
	MappingVersion   int       `json:"mapping_version"`
	EntryKey         string    `json:"entry_key"`
	RoleDefinitionID string    `json:"role_definition_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	RequestID        string    `json:"request_id,omitempty"`
	CorrelationID    string    `json:"correlation_id"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason,omitempty"`
	Attempts         int       `json:"attempts"`
	NextAttemptAt    time.Time `json:"next_attempt_at"`
	LastError        string    `json:"last_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// AssignmentResult is the recorded result of one attempt on an assignment.
type AssignmentResult struct {
	Status    string // the new status; PLANNED / REVOKE_PENDING keep it queued for a retry
	RequestID string
	Reason    string
	Error     string
	RetryAt   time.Time
}

// Exception is something provisioning could not settle by itself.
type Exception struct {
	ExceptionID  string     `json:"exception_id"`
	TenantID     string     `json:"tenant_id"`
	ConnectionID string     `json:"connection_id"`
	EventID      string     `json:"event_id,omitempty"`
	LinkID       string     `json:"link_id,omitempty"`
	AssignmentID string     `json:"assignment_id,omitempty"`
	Kind         string     `json:"kind"`
	Detail       string     `json:"detail,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	ResolvedBy   string     `json:"resolved_by,omitempty"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
	Resolution   string     `json:"resolution,omitempty"`
}

// UserView is a directory user as SCIM presents it.
type UserView struct {
	Link        Link
	DisplayName string
	Email       string
}

// ApplyInput is one decision to apply to a person, atomically.
type ApplyInput struct {
	TenantID       string
	ConnectionID   string
	EventID        string
	Source         string
	Issuer         string
	Operation      string
	Subject        string
	Username       string
	Email          string
	DisplayName    string
	Active         bool
	Attributes     Attributes
	MappingVersion int
	Desired        []Desired
	// ExpectedRev, when set, is the attributes_rev the change was computed against.
	ExpectedRev    *int
	Actor          string
	CorrelationID  string
	ReceivedAt     time.Time
	DeprovisionSLO time.Duration
	// ForceUpdate lets a reconcile deprovision without a revision check.
	LinkSource string
}

// ApplyResult is what applying produced.
type ApplyResult struct {
	Link          Link
	DisplayName   string
	Email         string
	Created       bool
	Reactivated   bool
	Deprovisioned bool
	Unchanged     bool
	Planned       int
	Revoking      int
}

// ProvStatus summarises a connection's provisioning.
type ProvStatus struct {
	ConnectionID       string         `json:"connection_id"`
	ConnectionStatus   string         `json:"connection_status"`
	ActiveMapping      *int           `json:"active_mapping_version"`
	Links              map[string]int `json:"links"`
	Events             map[string]int `json:"events"`
	Assignments        map[string]int `json:"assignments"`
	OpenExceptions     int            `json:"open_exceptions"`
	DeprovisionBacklog int            `json:"deprovision_backlog"`
	LastEventAt        *time.Time     `json:"last_event_at,omitempty"`
}

// LatencyRow is how long one deprovisioning took to take effect.
type LatencyRow struct {
	LinkID            string     `json:"link_id"`
	Subject           string     `json:"subject"`
	RequestedAt       time.Time  `json:"requested_at"`
	AppliedAt         *time.Time `json:"applied_at,omitempty"`
	SessionsEvictedAt *time.Time `json:"sessions_evicted_at,omitempty"`
	AccessRevokedAt   *time.Time `json:"access_revoked_at,omitempty"`
	DueAt             *time.Time `json:"due_at,omitempty"`
	// LocalMillis is request to the principal no longer being able to sign in; CompleteMillis is request to
	// every session ended and every assignment revoked. Nil while it is still pending.
	LocalMillis    *int64 `json:"local_ms,omitempty"`
	CompleteMillis *int64 `json:"complete_ms,omitempty"`
	Escalated      bool   `json:"escalated"`
}

// DeprovisionLatency is the answer to GetDeprovisionLatency.
type DeprovisionLatency struct {
	Rows        []LatencyRow `json:"rows"`
	Completed   int          `json:"completed"`
	Pending     int          `json:"pending"`
	Escalated   int          `json:"escalated"`
	MedianMS    int64        `json:"median_ms"`
	MaxMS       int64        `json:"max_ms"`
	SLOExceeded int          `json:"slo_exceeded"`
}

// SessionEvictor ends every live session of a principal.
type SessionEvictor interface {
	EvictAllForPrincipal(ctx context.Context, principalID, tenantID string) (int, error)
}

// ProvisionRepo is the persistence of provisioning. The Postgres implementation writes every change with its
// evidence and outbox event in one transaction.
type ProvisionRepo interface {
	// SCIM tokens (only the hash is held).
	CreateSCIMToken(ctx context.Context, tenantID, connectionID, tokenHash, actor, correlationID string) error
	RevokeSCIMTokens(ctx context.Context, tenantID, connectionID, actor, correlationID string) (int, error)
	// ValidSCIMToken reports whether the hash is the connection's active token, recording its use.
	ValidSCIMToken(ctx context.Context, tenantID, connectionID, tokenHash string) (bool, error)

	CreateMappingVersion(ctx context.Context, tenantID, connectionID string, cfg MappingConfig, actor, correlationID string) (*MappingVersion, error)
	DecideMappingVersion(ctx context.Context, tenantID, connectionID string, version int, approve bool,
		actor, fingerprint, reason, correlationID string) (*MappingVersion, error)
	// GetMappings returns the connection's mapping versions, newest first, and the approved one in force (or nil).
	GetMappings(ctx context.Context, tenantID, connectionID string) (active *MappingVersion, all []MappingVersion, err error)

	// BeginEvent records an event or returns the earlier one with the same dedupe key (created=false).
	BeginEvent(ctx context.Context, ev ProvEvent) (*ProvEvent, bool, error)
	DecideEvent(ctx context.Context, tenantID, eventID, status, reason, exceptionKind string, result map[string]any) error
	ListEvents(ctx context.Context, tenantID, connectionID, status string, limit int) ([]ProvEvent, error)

	ApplyIdentity(ctx context.Context, in ApplyInput) (*ApplyResult, error)
	GetUser(ctx context.Context, tenantID, connectionID, linkID string) (*UserView, error)
	FindUsers(ctx context.Context, tenantID, connectionID, attr, value string, limit int) ([]UserView, error)
	ActiveLinks(ctx context.Context, tenantID, connectionID string) ([]Link, error)

	// PendingAssignments returns a link's queued work now due or not.
	PendingAssignments(ctx context.Context, tenantID, linkID string) ([]Assignment, error)
	// ClaimDueAssignments is the worker's cross-tenant scan: due work, leased so replicas do not collide.
	ClaimDueAssignments(ctx context.Context, limit int, lease time.Duration) ([]Assignment, error)
	RecordAssignmentResult(ctx context.Context, tenantID, assignmentID string, res AssignmentResult) (*Assignment, error)

	MarkSessionsEvicted(ctx context.Context, tenantID, linkID string) error
	MarkAccessRevokedIfDone(ctx context.Context, tenantID, linkID string) (bool, error)
	LinksAwaitingEviction(ctx context.Context, limit int) ([]Link, error)
	LateDeprovisions(ctx context.Context, now time.Time, limit int) ([]Link, error)
	EscalateDeprovision(ctx context.Context, tenantID, linkID, detail string) (bool, error)

	AddException(ctx context.Context, ex Exception) error
	ListExceptions(ctx context.Context, tenantID, connectionID, status string, limit int) ([]Exception, error)
	ResolveException(ctx context.Context, tenantID, exceptionID, actor, resolution string) (*Exception, error)

	ProvisioningStatus(ctx context.Context, tenantID, connectionID string) (*ProvStatus, error)
	DeprovisionLatency(ctx context.Context, tenantID, connectionID string, limit int) ([]LatencyRow, error)
}
