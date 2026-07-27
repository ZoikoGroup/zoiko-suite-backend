// Package domain defines the authoritative domain types for
// evidence-requirements-svc.
//
// Per docs/architecture/03-microservices.md §8.6, this service determines
// what supporting evidence must exist before an action may be completed,
// and owns evidence preconditions, document requirements, signature
// requirements, supporting artifact rules, and evidence sufficiency logic.
// Its critical constraint: "No finalization path may skip required evidence
// states."
//
// Entity shape follows docs/architecture/04-data-model.md §7.1
// (EvidenceRequirement). LegalEntityID is an addition to that list — see
// context.md §11.1 for why (the data model omits it; doctrine requires it),
// and note it is a POINTER: NULL means the requirement applies tenant-wide.
package domain

import (
	"encoding/json"
	"time"
)

// Outcome is the result of one evidence sufficiency determination.
//
// Three values, not two. §8.6 names only two events
// (evidence.requirement.missing / .satisfied), but a third *response*
// outcome is required to stay honest: an empty requirement catalog is a
// legitimate data state, and reporting it as SATISFIED would make "nobody
// has configured this yet" indistinguishable from "verified complete".
// That collapse is exactly the defect shape found in tax-determination-svc,
// which returns a synthetic ZERO-TAX rule when its upstream has no match and
// so lets a transaction post with silently zeroed tax. Callers get a value
// they cannot mistake for verification instead.
type Outcome string

const (
	// OutcomeSatisfied means every effective requirement was matched.
	OutcomeSatisfied Outcome = "SATISFIED"
	// OutcomeMissing means at least one effective requirement was unmet.
	// Callers must treat this as a block on the action.
	OutcomeMissing Outcome = "MISSING"
	// OutcomeNoRequirementsDefined means no effective requirement rows exist
	// for this (domain_code, action_type). Not a failure, and explicitly not
	// SATISFIED.
	OutcomeNoRequirementsDefined Outcome = "NO_REQUIREMENTS_DEFINED"
)

// EvidenceRequirement is one effective-dated precondition on an action.
//
// Retired by setting EffectiveTo, never deleted and never flagged
// (doctrine: no soft-delete on material objects; effective end-dating only).
type EvidenceRequirement struct {
	EvidenceRequirementID string `json:"evidence_requirement_id"`
	TenantID              string `json:"tenant_id"`
	// LegalEntityID nil = applies tenant-wide. See package doc.
	LegalEntityID *string `json:"legal_entity_id,omitempty"`
	DomainCode    string  `json:"domain_code"`
	ActionType    string  `json:"action_type"`
	EvidenceType  string  `json:"evidence_type"`
	// RequirementPayload carries the sufficiency parameters as DATA
	// (minimum_count, artifact_subtype, description). This is the
	// extensibility seam that keeps the doctrine rule "no service may
	// hardcode a country, jurisdiction, currency, or tax-rule value as a
	// code constant, enum, or switch/case branch" satisfiable: a
	// jurisdiction that demands a notarised signature is a row, never a
	// code branch. See RequirementSpec.
	RequirementPayload json.RawMessage `json:"requirement_payload"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CorrelationID        string    `json:"correlation_id"`
}

// RequirementSpec is the decoded shape of EvidenceRequirement.
// RequirementPayload. Every field is optional; a completely empty payload
// means "one artifact of this evidence_type must be present".
//
// Deliberately small for v1. Extending sufficiency logic means adding
// fields here and handling them in the evaluator — it must never mean
// adding a jurisdiction-specific branch.
type RequirementSpec struct {
	// MinimumCount is how many matching artifacts must be present.
	// Zero/absent is treated as 1.
	MinimumCount int `json:"minimum_count,omitempty"`
	// ArtifactSubtype, when set, additionally requires that matching
	// artifacts declare this subtype.
	ArtifactSubtype string `json:"artifact_subtype,omitempty"`
	// Description is human-facing text surfaced in the unmet reason, so a
	// blocked caller learns what to go and produce.
	Description string `json:"description,omitempty"`
}

// EvidenceEvaluation is an append-only record of one determination — never
// UPDATEd, never DELETEd.
//
// This exists so the gate's own decisions are themselves auditable
// evidence, satisfying 03-microservices.md §17.6 ("Every Material Service
// Must Be Evidential") rather than only enforcing evidence on others.
type EvidenceEvaluation struct {
	EvaluationID  string  `json:"evaluation_id"`
	TenantID      string  `json:"tenant_id"`
	LegalEntityID string  `json:"legal_entity_id"`
	DomainCode    string  `json:"domain_code"`
	ActionType    string  `json:"action_type"`
	Outcome       Outcome `json:"outcome"`
	// UnmetPayload / PresentArtifactsPayload are frozen at decision time so
	// the record stays truthful even after the catalog changes underneath it.
	UnmetPayload            json.RawMessage `json:"unmet_payload"`
	PresentArtifactsPayload json.RawMessage `json:"present_artifacts_payload"`

	EvaluatedAt             time.Time `json:"evaluated_at"`
	EvaluatedForPrincipalID string    `json:"evaluated_for_principal_id"`
	CorrelationID           string    `json:"correlation_id"`
}

// ── wire types ───────────────────────────────────────────────────────────────

// PresentArtifact is one artifact the caller asserts exists.
//
// For EvidenceTypeSupportingDocument the ReferenceID is a document-vault-svc
// document_id and IS verified against that service before it counts — see
// internal/documentvault. Other evidence types are taken on the caller's
// word in v1 (context.md §11.2).
type PresentArtifact struct {
	EvidenceType    string `json:"evidence_type"`
	ReferenceID     string `json:"reference_id"`
	ArtifactSubtype string `json:"artifact_subtype,omitempty"`
}

// EvidenceTypeSupportingDocument is the one evidence type whose references
// point at document-vault-svc and are therefore verifiable. It is a
// reference-resolution rule, not a business-policy constant: evidence_type
// values themselves are free-form data supplied by the catalog, and nothing
// in this service enumerates the permitted set.
const EvidenceTypeSupportingDocument = "SUPPORTING_DOCUMENT"

// EvaluateRequest is the body of POST /v1/evidence/evaluate.
type EvaluateRequest struct {
	LegalEntityID    string            `json:"legal_entity_id"`
	DomainCode       string            `json:"domain_code"`
	ActionType       string            `json:"action_type"`
	PresentArtifacts []PresentArtifact `json:"present_artifacts"`
	CorrelationID    string            `json:"correlation_id"`
}

// UnmetRequirement names one requirement that was not satisfied. Each is
// reported individually with a reason — a bare boolean is not explainable
// evidence, and §8.7's "queryable by rule basis" expectation applies to
// this service's own decisions too.
type UnmetRequirement struct {
	EvidenceRequirementID string `json:"evidence_requirement_id"`
	EvidenceType          string `json:"evidence_type"`
	Reason                string `json:"reason"`
}

// EvaluateResponse is the body returned by POST /v1/evidence/evaluate.
type EvaluateResponse struct {
	EvaluationID  string             `json:"evaluation_id"`
	Outcome       Outcome            `json:"outcome"`
	Unmet         []UnmetRequirement `json:"unmet"`
	EvaluatedAt   time.Time          `json:"evaluated_at"`
	CorrelationID string             `json:"correlation_id"`
}

// CreateRequirementRequest is the body of POST /v1/admin/evidence-requirements.
type CreateRequirementRequest struct {
	TenantID           string          `json:"tenant_id"`
	LegalEntityID      *string         `json:"legal_entity_id,omitempty"`
	DomainCode         string          `json:"domain_code"`
	ActionType         string          `json:"action_type"`
	EvidenceType       string          `json:"evidence_type"`
	RequirementPayload json.RawMessage `json:"requirement_payload,omitempty"`
	// EffectiveFrom defaults to now when absent.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	CorrelationID string     `json:"correlation_id"`
}

// EndDateRequirementRequest is the body of
// POST /v1/admin/evidence-requirements/{id}/end-date. EffectiveTo defaults
// to now when absent.
type EndDateRequirementRequest struct {
	EffectiveTo *time.Time `json:"effective_to,omitempty"`
	Reason      string     `json:"reason"`
}

// ListRequirementsFilter holds filters for querying the catalog. TenantID is
// required; the rest are optional.
type ListRequirementsFilter struct {
	TenantID      string
	LegalEntityID string
	DomainCode    string
	ActionType    string
	// AsOf restricts results to requirements effective at that instant.
	// Zero value means "no effective-date filter — return all, including
	// retired ones", which is what an auditor reviewing history needs.
	AsOf time.Time
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRequirementNotFound = errorString("evidence requirement not found")
	ErrEvaluationNotFound  = errorString("evidence evaluation not found")
	ErrStoreUnavailable    = errorString("evidence requirements store unavailable")

	// ErrAlreadyRetired is returned when end-dating a requirement that
	// already carries an effective_to. Surfaced as 422, never a silent
	// no-op — invoice-approval-svc's non-atomic read-then-write is the
	// anti-pattern this guards against.
	ErrAlreadyRetired = errorString("evidence requirement is already retired")

	ErrAuthorizationDenied             = errorString("authorization denied for this evidence requirement action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a request carries no resolved
	// identity (no X-Principal-Id header) — it never passed
	// gateway-auth-svc's ForwardAuth verification. Fail closed.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrTenantMissing is returned when X-Tenant-Id is absent. Never
	// defaulted to a placeholder tenant: offboarding-severance-svc and
	// workforce-compliance-svc silently fall back to "default-tenant" here,
	// which is a real cross-tenant defect.
	ErrTenantMissing = errorString("tenant scope missing")

	// Document-vault verification errors. Fail closed on all of them: an
	// artifact that cannot be confirmed to exist does not count as evidence.
	ErrDocumentNotFound             = errorString("referenced document not found")
	ErrDocumentMismatch             = errorString("referenced document belongs to a different tenant or legal entity")
	ErrDocumentServiceUnavailable   = errorString("document-vault-svc unavailable")
)
