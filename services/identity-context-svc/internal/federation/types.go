// Package federation is INT-10, the tenant IdP adapter: governed federation connections, the OIDC
// token verification a federated login rests on, external-subject links, and (in provision.go) the
// SCIM provisioning pipeline.
//
// What it owns is the relationship with a tenant's identity provider and the evidence of it. What it
// refuses to own is anything that would let an IdP grant authority: it never writes a role, it never
// treats an email address as an identity, and it has no path that skips signature verification.
package federation

import (
	"errors"
	"time"
)

// Connection statuses. A connection with no approved version is PENDING_APPROVAL; DEGRADED is an
// approved connection whose signing keys could not be refreshed; SUSPENDED is an operator's stop;
// REVOKED is final.
const (
	ConnPendingApproval = "PENDING_APPROVAL"
	ConnActive          = "ACTIVE"
	ConnDegraded        = "DEGRADED"
	ConnSuspended       = "SUSPENDED"
	ConnRevoked         = "REVOKED"
)

// Version statuses.
const (
	VerPending    = "PENDING_APPROVAL"
	VerApproved   = "APPROVED"
	VerRejected   = "REJECTED"
	VerSuperseded = "SUPERSEDED"
)

// Link statuses and sources.
const (
	LinkActive        = "ACTIVE"
	LinkSuspended     = "SUSPENDED"
	LinkDeprovisioned = "DEPROVISIONED"

	SourceSCIM  = "SCIM"
	SourceJIT   = "JIT"
	SourceAdmin = "ADMIN"
)

// Outage behaviours.
const (
	OutageFailClosed = "FAIL_CLOSED"
	OutageCachedKeys = "CACHED_KEYS"
)

// Login outcomes.
const (
	OutcomeSucceeded = "SUCCEEDED"
	OutcomeDenied    = "DENIED"
)

// Connection is the named relationship with one tenant IdP.
type Connection struct {
	ConnectionID  string    `json:"connection_id"`
	TenantID      string    `json:"tenant_id"`
	Name          string    `json:"name"`
	Protocol      string    `json:"protocol"`
	Status        string    `json:"status"`
	StatusReason  string    `json:"status_reason,omitempty"`
	ActiveVersion *int      `json:"active_version"`
	LatestVersion int       `json:"latest_version"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Version is one frozen trust configuration of a connection.
type Version struct {
	ConnectionID   string     `json:"connection_id"`
	TenantID       string     `json:"tenant_id"`
	Version        int        `json:"version"`
	Status         string     `json:"status"`
	Config         Config     `json:"config"`
	Fingerprint    string     `json:"fingerprint"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
}

// ConnectionDetail is a connection with its versions, newest first.
type ConnectionDetail struct {
	Connection
	Versions []Version `json:"versions"`
}

// Link ties an external subject to the principal that holds access here.
type Link struct {
	LinkID          string     `json:"link_id"`
	TenantID        string     `json:"tenant_id"`
	ConnectionID    string     `json:"connection_id"`
	Issuer          string     `json:"issuer"`
	Subject         string     `json:"subject"`
	PrincipalID     string     `json:"principal_id"`
	Status          string     `json:"status"`
	Source          string     `json:"link_source"`
	LinkedBy        string     `json:"linked_by"`
	CreatedAt       time.Time  `json:"created_at"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
	DeprovisionedAt *time.Time `json:"deprovisioned_at,omitempty"`
	Username        string     `json:"username,omitempty"`
	Attributes      Attributes `json:"attributes,omitempty"`
	AttributesRev   int        `json:"-"`

	DeprovisionRequestedAt *time.Time `json:"deprovision_requested_at,omitempty"`
	DeprovisionDueAt       *time.Time `json:"deprovision_due_at,omitempty"`
	SessionsEvictedAt      *time.Time `json:"sessions_evicted_at,omitempty"`
	AccessRevokedAt        *time.Time `json:"access_revoked_at,omitempty"`
	DeprovisionEscalatedAt *time.Time `json:"deprovision_escalated_at,omitempty"`
}

// LoginRecord is the evidence of a federated login whose token signature verified.
type LoginRecord struct {
	LogID         string    `json:"log_id"`
	TenantID      string    `json:"tenant_id"`
	ConnectionID  string    `json:"connection_id"`
	ConfigVersion int       `json:"config_version"`
	Issuer        string    `json:"issuer"`
	Subject       string    `json:"subject"`
	PrincipalID   string    `json:"principal_id,omitempty"`
	Outcome       string    `json:"outcome"`
	Reason        string    `json:"reason"`
	KeyID         string    `json:"key_id,omitempty"`
	MFAAsserted   bool      `json:"mfa_asserted"`
	DegradedKeys  bool      `json:"degraded_keys"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// KeySetRecord is the last JWKS fetched for a connection.
type KeySetRecord struct {
	ConnectionID        string
	TenantID            string
	JWKSURI             string
	JWKS                []byte
	KeyCount            int
	FetchedAt           time.Time
	LastAttemptAt       time.Time
	LastError           string
	ConsecutiveFailures int
}

// Errors. Handlers map them to status codes; none is ever shown to an unauthenticated login caller,
// who sees only the uniform rejection.
var (
	ErrNotFound         = errors.New("federation connection not found")
	ErrInvalid          = errors.New("federation request is invalid")
	ErrConflict         = errors.New("federation state conflict")
	ErrSelfApproval     = errors.New("a version cannot be decided by the person who created it")
	ErrFingerprint      = errors.New("the approval fingerprint does not match the version")
	ErrNotPending       = errors.New("the version is not awaiting a decision")
	ErrTransition       = errors.New("the connection cannot make that transition")
	ErrKeysUnavailable  = errors.New("signing keys could not be obtained")
	ErrLinkNotFound     = errors.New("external subject link not found")
	ErrLinkConflict     = errors.New("the external subject or principal is already linked")
	ErrUnknownPrincipal = errors.New("principal not found")
)
