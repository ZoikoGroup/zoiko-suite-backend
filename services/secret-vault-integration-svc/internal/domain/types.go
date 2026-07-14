// Package domain contains the authoritative domain types for
// secret-vault-integration-svc.
//
// This service never stores an actual secret value — only policy
// metadata (who may access what), lease metadata (who was granted
// access and when it expires), and audit records (every request,
// grant, denial, revocation, and rotation). Real secret material lives
// behind the VaultBackend interface (internal/vault), never in these
// structs or in Postgres. See context.md §1 and §7.6.
//
// All type/status discriminator fields are plain strings — no Go enums,
// iota, or switch/case branches in validation logic. New secret_class or
// event_type values are added via data only (context.md §2).
package domain

import (
	"encoding/json"
	"time"
)

// SecretPolicy is the authoritative named container for a vault
// integration policy. It owns no access-rule content itself — that
// lives on its SecretPolicyVersion rows. No soft-delete, no
// UPDATE/DELETE: a policy row is immutable once created.
type SecretPolicy struct {
	SecretPolicyID string `json:"secret_policy_id"`

	// SecretClass is data-driven (context.md §2): DATABASE_CREDENTIAL,
	// INTEGRATION_TOKEN, BANK_CREDENTIAL, ESIGNATURE_CREDENTIAL,
	// PRIVATE_KEY, ENCRYPTION_MATERIAL_REFERENCE, API_SIGNING_SECRET,
	// SERVICE_TO_SERVICE_TRUST_MATERIAL.
	SecretClass string `json:"secret_class"`

	// SecretPath is the opaque reference in the underlying vault
	// backend — never the secret value itself. This is the table's
	// unique natural key on its own (context.md §7.1's design
	// correction — not (secret_class, secret_path)).
	SecretPath string `json:"secret_path"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	DataClassification   string    `json:"data_classification"`
}

// SecretPolicyVersion is an effective-dated, state-machined access-rule
// record scoped to a policy and optionally a tenant/legal entity.
//
// No UPDATE/DELETE: a change is always either a new DRAFT version or a
// version_status transition (DRAFT -> ACTIVE -> SUPERSEDED, or -> RETIRED).
type SecretPolicyVersion struct {
	SecretPolicyVersionID string `json:"secret_policy_version_id"`
	SecretPolicyID        string `json:"secret_policy_id"`

	// TenantID nil means this version applies globally.
	TenantID *string `json:"tenant_id"`
	// LegalEntityID nil means this version applies to the whole tenant
	// (or globally, if TenantID is also nil).
	LegalEntityID *string `json:"legal_entity_id"`

	// AllowedWorkloadIDs is a JSON array of workload/service/principal
	// identifiers permitted to broker this secret in this scope.
	AllowedWorkloadIDs json.RawMessage `json:"allowed_workload_ids"`

	MaxLeaseDurationSeconds int `json:"max_lease_duration_seconds"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// VersionStatus: DRAFT | ACTIVE | SUPERSEDED | RETIRED.
	VersionStatus string `json:"version_status"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// ApplicableSecretPolicyVersion is a SecretPolicyVersion enriched with
// its owning policy's SecretClass/SecretPath. Returned by the "get
// applicable secret policy set" query and used internally by the broker
// to resolve a request without a second round trip.
type ApplicableSecretPolicyVersion struct {
	SecretPolicyVersion
	SecretClass string `json:"secret_class"`
	SecretPath  string `json:"secret_path"`
}

// SecretLease is a durable grant record — effective-dated and revocable,
// never hard-deleted. Only ever created for a real grant; denials never
// become leases (see SecretAccessAuditLog).
type SecretLease struct {
	LeaseID string `json:"lease_id"`

	// RequestID is caller-supplied and is the idempotency/dedup key.
	RequestID string `json:"request_id"`

	SecretPolicyVersionID string `json:"secret_policy_version_id"`

	// Denormalized from the resolved policy at grant time.
	SecretClass string `json:"secret_class"`
	SecretPath  string `json:"secret_path"`

	RequestedByPrincipalID string  `json:"requested_by_principal_id"`
	TenantID               *string `json:"tenant_id"`
	LegalEntityID          *string `json:"legal_entity_id"`

	// Status: GRANTED | EXPIRED | REVOKED.
	Status string `json:"status"`

	GrantedAt time.Time  `json:"granted_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`

	CorrelationID string    `json:"correlation_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// SecretAccessAuditLog is one immutable evidence record of a request,
// grant, denial, revocation, or rotation. Append-only — no UPDATE, no
// DELETE, ever, same guarantee as governance-decision-log-svc's
// GovernanceDecision.
type SecretAccessAuditLog struct {
	AuditLogID string `json:"audit_log_id"`

	// EventType: REQUESTED | GRANTED | DENIED | REVOKED | ROTATED.
	EventType string `json:"event_type"`

	SecretClass string `json:"secret_class"`
	SecretPath  string `json:"secret_path"`

	RequestedByPrincipalID string  `json:"requested_by_principal_id"`
	TenantID               *string `json:"tenant_id"`
	LegalEntityID          *string `json:"legal_entity_id"`

	// LeaseID is nil for REQUESTED/DENIED — nothing was granted to
	// reference.
	LeaseID *string `json:"lease_id"`

	// SecretPolicyVersionID is nil for DENIED when no policy existed at
	// all for that path/scope.
	SecretPolicyVersionID *string `json:"secret_policy_version_id"`

	// RequestID is only populated (and only deduped) for ROTATED
	// entries — see Store.Rotate's doc comment.
	RequestID *string `json:"request_id"`

	OutcomeDetail string `json:"outcome_detail"`
	CorrelationID string `json:"correlation_id"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateSecretPolicyParams holds input parameters for creating a secret
// policy.
type CreateSecretPolicyParams struct {
	SecretPolicyID       string
	SecretClass          string
	SecretPath           string
	CreatedByPrincipalID string
	DataClassification   string
}

// CreateSecretPolicyVersionParams holds input parameters for creating a
// secret policy version. New versions are always created in DRAFT status.
type CreateSecretPolicyVersionParams struct {
	SecretPolicyVersionID   string
	SecretPolicyID          string
	TenantID                *string
	LegalEntityID           *string
	AllowedWorkloadIDs      []byte
	MaxLeaseDurationSeconds int
	EffectiveFrom           time.Time
	EffectiveTo             *time.Time
	CreatedByPrincipalID    string
}

// BrokerParams holds input parameters for POST /v1/secrets/broker.
type BrokerParams struct {
	RequestID              string
	SecretPath             string
	TenantID               *string
	LegalEntityID          *string
	RequestedByPrincipalID string
	CorrelationID          string
}

// BrokerResult is the outcome of a broker request — always returned,
// whether granted or denied, so the handler can respond and log
// consistently regardless of outcome.
type BrokerResult struct {
	Granted   bool
	Lease     *SecretLease
	LeaseToken string
}

// RotateParams holds input parameters for
// POST /v1/secret-policies/{id}/rotate.
type RotateParams struct {
	RequestID            string
	SecretPolicyID       string
	CorrelationID        string
	RotatedByPrincipalID string
}

// CreateLeaseParams holds input parameters for inserting a granted lease.
type CreateLeaseParams struct {
	RequestID              string
	SecretPolicyVersionID  string
	SecretClass            string
	SecretPath             string
	RequestedByPrincipalID string
	TenantID               *string
	LegalEntityID          *string
	ExpiresAt              time.Time
	CorrelationID          string
}

// RecordAuditEntryParams holds input parameters for a single append-only
// audit log write.
type RecordAuditEntryParams struct {
	EventType              string
	SecretClass            string
	SecretPath             string
	RequestedByPrincipalID string
	TenantID               *string
	LegalEntityID          *string
	LeaseID                *string
	SecretPolicyVersionID  *string
	RequestID              *string
	OutcomeDetail          string
	CorrelationID          string
}

// ── errors ───────────────────────────────────────────────────────────────────

// ErrSecretPolicyNotFound is returned when a secret_policy_id does not exist.
var ErrSecretPolicyNotFound = errorString("secret policy not found")

// ErrSecretPolicyVersionNotFound is returned when a
// secret_policy_version_id does not exist.
var ErrSecretPolicyVersionNotFound = errorString("secret policy version not found")

// ErrLeaseNotFound is returned when a lease_id does not exist.
var ErrLeaseNotFound = errorString("secret lease not found")

// ErrInvalidTransition is returned when a version_status or lease status
// transition is illegal per the state machine.
var ErrInvalidTransition = errorString("invalid status transition")

// ErrConflict is returned when an idempotent creation request matches an
// existing record's dedup key but has differing attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, never as "not found"
// or, worse, as an implicit grant.
var ErrStoreUnavailable = errorString("secret vault store unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
