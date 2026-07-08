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

	// RulePayload holds the actual rule content. json.RawMessage so it is
	// inlined in API responses as JSON, not base64-encoded bytes.
	RulePayload json.RawMessage `json:"rule_payload"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// VersionStatus: DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum.
	VersionStatus string `json:"version_status"`

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

type errorString string

func (e errorString) Error() string { return string(e) }
