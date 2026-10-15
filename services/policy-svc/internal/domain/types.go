// Package domain contains the authoritative domain types for policy-svc.
//
// All type/status discriminator fields are plain strings — no Go enums,
// iota, or switch/case branches in validation logic. New policy_type or
// version_status values are added via data only; no code change required
// (per .agents/rules/doctrine.md — same doctrine as jurisdiction-rules-svc's
// jurisdiction_type/rule_domain fields).
package domain

import (
	"encoding/json"
	"time"
)

// Policy is the authoritative named container for a policy definition.
// It owns no rule content itself — content lives on its PolicyVersion rows.
// No soft-delete, no UPDATE/DELETE: a policy row is immutable once created.
type Policy struct {
	PolicyID string `json:"policy_id"`

	// PolicyCode is a stable, human-readable identifier and the idempotent
	// creation dedup key — DATA ONLY, never used as a code switch/case.
	PolicyCode string `json:"policy_code"`

	PolicyName string `json:"policy_name"`

	// PolicyType is a VARCHAR tag stored as data: e.g. APPROVAL_THRESHOLD,
	// SPEND_CONTROL, SOD_RULE, SIGNATORY_MATRIX. New types require a data
	// migration only, never a code change to this type or to store queries.
	// Only the evaluation handler switches on this value, and only for the
	// types it actually implements (v1: APPROVAL_THRESHOLD only).
	PolicyType string `json:"policy_type"`

	// TenantID: nullable, NULL means platform-wide policy family (not tenant-owned).
	// Per 04-data-model.md §7.1: Policy is tenant-owned, but platform-wide policies exist.
	TenantID *string `json:"tenant_id,omitempty"`

	// PolicyStatus: DRAFT | ACTIVE | RETIRED (policy-level lifecycle, not version-level).
	// Per 04-data-model.md §7.1: policy_status tracks the policy family's lifecycle.
	PolicyStatus string `json:"policy_status"`

	// VersioningMode: SIMPLE | BRANCHED | FORMAL — controls how versions are created/managed.
	// Per 04-data-model.md §7.1: versioning_mode controls the versioning strategy.
	VersioningMode string `json:"versioning_mode"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// PolicyVersion is an effective-dated, state-machined rule-content record
// scoped to a policy and optionally a tenant/legal entity.
//
// rule_payload's shape depends on the owning Policy's PolicyType — see the
// handler package for the APPROVAL_THRESHOLD evaluation contract.
//
// No UPDATE/DELETE: a change is always either a new DRAFT version or a
// version_status transition (DRAFT -> ACTIVE -> SUPERSEDED, or -> RETIRED).
type PolicyVersion struct {
	PolicyVersionID string `json:"policy_version_id"`
	PolicyID        string `json:"policy_id"`

	// TenantID nil means this version applies globally, across all tenants.
	TenantID *string `json:"tenant_id"`

	// LegalEntityID nil means this version applies to the whole tenant (or
	// globally, if TenantID is also nil).
	LegalEntityID *string `json:"legal_entity_id"`

	// ScopeType is GLOBAL | TENANT | LEGAL_ENTITY — the explicit, named form
	// of what TenantID/LegalEntityID's nullness already encodes. Per
	// docs/original_doc/zoiko_suite_doc7.txt §F1 ("'GLOBAL' is an explicit
	// scope, not a default"), enforced consistent with TenantID/LegalEntityID
	// by a DB CHECK constraint (migration 000003) — this field is derived,
	// never set independently of them.
	ScopeType string `json:"scope_type"`

	// RulePayload holds the actual rule content. json.RawMessage so it is
	// inlined in API responses as JSON, not base64-encoded bytes.
	RulePayload json.RawMessage `json:"rule_payload"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// VersionStatus: DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum.
	VersionStatus string `json:"version_status"`

	// VersionNumber: sequential integer per policy, for human readability.
	// Per 04-data-model.md §7.1.
	VersionNumber int `json:"version_number"`

	// Source: where this version originated (e.g. "internal", "imported", "migrated").
	// Per GCP §18 / V-001 §7.
	Source string `json:"source"`

	// Rationale: human-readable explanation for why this version was created.
	// Per GCP §18 / V-001 §7.
	Rationale string `json:"rationale"`

	// ArtifactDigest: SHA256 of the rule_payload for integrity verification.
	// Per GCP §18 / V-001 §8.1.
	ArtifactDigest string `json:"artifact_digest"`

	// KnownFrom: when this version became known to the platform (decision_as_of / known_at).
	// Per V-001 §8.1.
	KnownFrom time.Time `json:"known_from"`

	// ActivatedByPrincipalID is the principal who performed this version's
	// DRAFT->ACTIVE transition. Nil until the version is activated for the
	// first time; never overwritten afterwards, including when this
	// version is later superseded — its own activation history stands.
	ActivatedByPrincipalID *string `json:"activated_by_principal_id"`

	// ActivatedAt is when this version's DRAFT->ACTIVE transition happened.
	// Nil until activation, set exactly once, same lifecycle as
	// ActivatedByPrincipalID.
	ActivatedAt *time.Time `json:"activated_at"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// Scope type constants for PolicyVersion.ScopeType.
const (
	ScopeTypeGlobal      = "GLOBAL"
	ScopeTypeTenant      = "TENANT"
	ScopeTypeLegalEntity = "LEGAL_ENTITY"
)

// DeriveScopeType returns the explicit scope name for a given
// tenantID/legalEntityID nullness pattern — the single source of truth the
// store uses both when inserting a new version and when scanning one back,
// so the derivation can never drift between the two.
func DeriveScopeType(tenantID, legalEntityID *string) string {
	switch {
	case tenantID == nil:
		return ScopeTypeGlobal
	case legalEntityID == nil:
		return ScopeTypeTenant
	default:
		return ScopeTypeLegalEntity
	}
}

// ApplicablePolicyVersion is a PolicyVersion enriched with its owning
// policy's PolicyCode. Returned by the "get applicable policy set" query
// (GET /v1/policies) and used internally by evaluation to build a
// human-readable RuleBasis without a second round trip.
type ApplicablePolicyVersion struct {
	PolicyVersion
	PolicyCode string `json:"policy_code"`
}

// CreatePolicyParams holds input parameters for creating a policy.
type CreatePolicyParams struct {
	PolicyID             string `json:"policy_id"`
	PolicyCode           string `json:"policy_code"`
	PolicyName           string `json:"policy_name"`
	PolicyType           string `json:"policy_type"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
}

// CreatePolicyVersionParams holds input parameters for creating a policy
// version. New versions are always created in DRAFT status; activation is
// a separate transition (see Store.ActivateVersion).
type CreatePolicyVersionParams struct {
	PolicyVersionID      string     `json:"policy_version_id"`
	PolicyID             string     `json:"policy_id"`
	TenantID             *string    `json:"tenant_id"`
	LegalEntityID        *string    `json:"legal_entity_id"`
	RulePayload          []byte     `json:"rule_payload"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// ErrPolicyNotFound is returned when a policy_id does not exist.
var ErrPolicyNotFound = errorString("policy not found")

// ErrPolicyVersionNotFound is returned when a policy_version_id does not exist.
var ErrPolicyVersionNotFound = errorString("policy version not found")

// ErrInvalidTransition is returned when a version_status transition is
// illegal per the state machine (e.g. activating a non-DRAFT version).
var ErrInvalidTransition = errorString("invalid policy version status transition")

// ErrConflict is returned when an idempotent creation request matches an
// existing record's dedup key but has differing attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("policy store unavailable")

// ErrAuthorizationDenied is returned when authorization-svc answers DENIED
// for a policy mutation (403 Forbidden).
var ErrAuthorizationDenied = errorString("authorization denied")

// ErrAuthorizationServiceUnavailable is returned when no authorization
// decision could be obtained (503). Callers must fail closed — a policy
// mutation never proceeds without a positive decision.
var ErrAuthorizationServiceUnavailable = errorString("authorization service unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
