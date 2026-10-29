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
	// RecordVersion is the optimistic-concurrency token (migration 000009).
	RecordVersion int64 `json:"record_version"`
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

	// RowVersion counts lifecycle transitions; it is the value a caller
	// passes back as expected_version (or If-Match) on a stateful command.
	RowVersion int `json:"row_version"`

	SubmittedByPrincipalID *string    `json:"submitted_by_principal_id,omitempty"`
	SubmittedAt            *time.Time `json:"submitted_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovalReference      *string    `json:"approval_reference,omitempty"`

	// WithdrawnAt is when a suspension or retirement was recorded. A
	// resolution whose knowledge cut is at or after it ignores the version.
	WithdrawnAt           *time.Time `json:"withdrawn_at,omitempty"`
	SupersededAt          *time.Time `json:"superseded_at,omitempty"`
	SupersededByVersionID *string    `json:"superseded_by_version_id,omitempty"`

	// Emergency suspension and its independent review (GOV-05 negative path
	// #3: "evidenced and reviewed").
	SuspendedByPrincipalID          *string    `json:"suspended_by_principal_id,omitempty"`
	SuspensionReviewedByPrincipalID *string    `json:"suspension_reviewed_by_principal_id,omitempty"`
	SuspensionReviewedAt            *time.Time `json:"suspension_reviewed_at,omitempty"`
	SuspensionReviewOutcome         *string    `json:"suspension_review_outcome,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// Version statuses — the GOV-05 state model
// (Draft/Review/Approved/Scheduled/Active/Suspended/Superseded/Retired).
// Plain strings, matching the VARCHAR column and its CHECK constraint.
const (
	StatusDraft      = "DRAFT"
	StatusReview     = "REVIEW"
	StatusApproved   = "APPROVED"
	StatusScheduled  = "SCHEDULED"
	StatusActive     = "ACTIVE"
	StatusSuspended  = "SUSPENDED"
	StatusSuperseded = "SUPERSEDED"
	StatusRetired    = "RETIRED"
)

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
	PolicyID   string `json:"policy_id"`
	PolicyCode string `json:"policy_code"`
	PolicyName string `json:"policy_name"`
	PolicyType string `json:"policy_type"`

	// TenantID is the owning tenant; nil creates a platform-wide family.
	TenantID *string `json:"tenant_id"`

	CreatedByPrincipalID string `json:"created_by_principal_id"`

	Meta EventMeta `json:"-"`
}

// EventMeta is the request context a store write stamps onto the events it
// enqueues in the same transaction.
type EventMeta struct {
	CorrelationID  string
	CausationID    string
	IdempotencyKey string
	ActorID        string
	// JurisdictionContext is the request's X-Jurisdiction-Context, carried
	// on the event envelope (Doc 03 §19).
	JurisdictionContext string
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
	Source               string     `json:"source"`
	Rationale            string     `json:"rationale"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`

	// RequestKey is the create request's Idempotency-Key: an identical
	// retry with the same key replays the stored version.
	RequestKey string `json:"-"`

	Meta EventMeta `json:"-"`
}

// TransitionCommand is one GOV-05 lifecycle command against a version.
type TransitionCommand struct {
	PolicyVersionID string
	// Command is one of the Command* constants.
	Command string
	ActorID string
	// ExpectedVersion, when non-nil, must equal the version's row_version.
	ExpectedVersion   *int
	Reason            string
	ApprovalReference string
	// Outcome is the review's conclusion for CommandReviewSuspension:
	// SuspensionUpheld or SuspensionOverturned.
	Outcome string
	Meta    EventMeta
}

// Lifecycle commands (GOV-05 commands). Supersession is not a command of its
// own: activating a successor supersedes the active version in its scope.
const (
	CommandSubmitReview = "SUBMIT_REVIEW"
	CommandApprove      = "APPROVE"
	CommandActivate     = "ACTIVATE"
	CommandSchedule     = "SCHEDULE"
	CommandSuspend      = "SUSPEND"
	CommandRetire       = "RETIRE"
	// CommandReviewSuspension records the independent review of an emergency
	// suspension. The status stays SUSPENDED: an overturned suspension is
	// put right by activating a version, never by un-suspending one.
	CommandReviewSuspension = "REVIEW_SUSPENSION"
)

// Suspension review outcomes.
const (
	SuspensionUpheld     = "UPHELD"
	SuspensionOverturned = "OVERTURNED"
)

// TransitionResult is what a lifecycle command did.
type TransitionResult struct {
	Version *PolicyVersion
	// Superseded lists versions this command moved to SUPERSEDED.
	Superseded []*PolicyVersion
	// Transitioned is false for an idempotent repeat of a command that had
	// already taken effect.
	Transitioned bool
}

// VersionTransition is one row of the append-only transition log.
type VersionTransition struct {
	TransitionID      int64     `json:"transition_id"`
	PolicyVersionID   string    `json:"policy_version_id"`
	FromStatus        *string   `json:"from_status"`
	ToStatus          string    `json:"to_status"`
	ActorID           string    `json:"actor_id"`
	Reason            *string   `json:"reason,omitempty"`
	ApprovalReference *string   `json:"approval_reference,omitempty"`
	CorrelationID     *string   `json:"correlation_id,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// Evaluation is the durable evidence of one policy evaluation (V-001
// PDC-I-17): what was asked, what was resolved, and what was decided.
type Evaluation struct {
	EvaluationID           string          `json:"evidence_id"`
	TenantID               string          `json:"tenant_id"`
	DecisionID             string          `json:"decision_id"`
	LegalEntityID          *string         `json:"legal_entity_id,omitempty"`
	PolicyType             string          `json:"policy_type"`
	RequestedByPrincipalID string          `json:"requested_by_principal_id,omitempty"`
	InputFingerprint       string          `json:"input_fingerprint"`
	ActionContext          json.RawMessage `json:"action_context"`
	AsOf                   time.Time       `json:"as_of"`
	KnownAt                time.Time       `json:"known_at"`
	Result                 string          `json:"result"`
	Outcome                string          `json:"outcome"`
	PolicyID               string          `json:"policy_id"`
	PolicyVersionID        string          `json:"policy_version_id"`
	PolicyCode             string          `json:"policy_code"`
	ArtifactDigest         string          `json:"artifact_digest"`
	RuleBasis              string          `json:"rule_basis"`
	SelectedRuleIDs        []string        `json:"selected_rule_ids"`
	EvaluatorVersion       string          `json:"evaluator_version"`
	CorrelationID          string          `json:"correlation_id"`
	CausationID            string          `json:"causation_id,omitempty"`
	RequestID              string          `json:"request_id,omitempty"`
	EvaluatedAt            time.Time       `json:"evaluated_at"`

	// LedgerStatus is PENDING until governance-decision-log-svc has the
	// record, then RECORDED; FAILED if that service refused it for good.
	LedgerStatus string `json:"ledger_status"`
}

// Ledger delivery states reported on an Evaluation.
const (
	LedgerPending  = "PENDING"
	LedgerRecorded = "RECORDED"
	LedgerFailed   = "FAILED"
)

// ErrPolicyNotFound is returned when a policy_id does not exist.
var ErrPolicyNotFound = errorString("policy not found")

// ErrPolicyVersionNotFound is returned when a policy_version_id does not exist.
var ErrPolicyVersionNotFound = errorString("policy version not found")

// ErrInvalidTransition is returned when a version_status transition is
// illegal per the state machine (e.g. approving a version not in REVIEW).
var ErrInvalidTransition = errorString("invalid policy version status transition")

// ErrConflict is returned when an idempotent creation request matches an
// existing record's dedup key but has differing attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("policy store unavailable")

// ErrApprovalRequired is returned when a version is activated before an
// independent approver has approved it (GOV-05 SoD: "production activation
// requires approved artifact").
var ErrApprovalRequired = errorString("version must be approved before activation")

// ErrSoDConflict is returned when the same principal would act as both maker
// and checker on a version.
var ErrSoDConflict = errorString("segregation of duties conflict")

// ErrConcurrencyConflict is returned when expected_version does not match the
// stored row_version, or the version's state no longer allows the command.
var ErrConcurrencyConflict = errorString("concurrency conflict")

// ErrPolicyConflicted is returned when two distinct policies of one type
// would both govern the same scope and time (GOV-05 negative path #2).
var ErrPolicyConflicted = errorString("conflicting policies")

// ErrIdempotencyMismatch is returned when a replayed key or id arrives with
// different content than the request that first used it.
var ErrIdempotencyMismatch = errorString("idempotency key reused with different content")

// ErrIntegrity is returned when a stored payload no longer matches its
// recorded digest.
var ErrIntegrity = errorString("artifact digest mismatch")

// ErrInvalidScope is returned when a version's scope is incompatible with its
// policy family's owner.
var ErrInvalidScope = errorString("invalid scope for this policy family")

// ErrEvaluationNotFound is returned when no evaluation evidence matches.
var ErrEvaluationNotFound = errorString("evaluation not found")

// ErrAuthorizationDenied is returned when authorization-svc answers DENIED
// for a policy mutation (403 Forbidden).
var ErrAuthorizationDenied = errorString("authorization denied")

// ErrAuthorizationServiceUnavailable is returned when no authorization
// decision could be obtained (503). Callers must fail closed — a policy
// mutation never proceeds without a positive decision.
var ErrAuthorizationServiceUnavailable = errorString("authorization service unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
