// Package domain contains the authoritative domain types for obligations-svc.
//
// obligation_source_type, obligation_type, obligation_status, and
// severity_level are all plain strings — no Go enums, iota, or switch/case
// branches in validation logic. New values are added via data only, no code
// change required, same doctrine as policy-svc's policy_type and
// jurisdiction-rules-svc's jurisdiction_type/rule_domain fields. Only the
// obligation_status state-machine transition check switches on status
// values, since that's an actual state machine, not an open-ended tag.
package domain

import "time"

// Obligation tracks a single statutory, regulatory, contractual, or internal
// policy obligation linked to an operational action.
//
// Critical constraint (03-microservices.md §8.5): every obligation must be
// entity-bound (LegalEntityID) and jurisdiction-bound (JurisdictionID) —
// neither field may ever be empty.
//
// Critical enhancement — "Atomic Linking": every obligation must be able to
// point to its originating source (a contract clause, filing rule, policy
// mandate, or jurisdiction rule reference). SourceReference is required for
// exactly this reason and must never be empty.
//
// Spec §8.5: "Owns: obligation-to-workflow mappings" — workflow_id links to
// a workflow & approvals service instance.
//
// No hard-delete: an obligation is closed via an ObligationStatus transition
// to CLOSED (ClosedAt stamped), never deleted.
type Obligation struct {
	ObligationID string `json:"obligation_id"`

	LegalEntityID  string `json:"legal_entity_id"`
	JurisdictionID string `json:"jurisdiction_id"`

	// WorkflowID links this obligation to a workflow instance in the
	// workflow & approvals service. Optional — not all obligations are
	// workflow-driven.
	WorkflowID *string `json:"workflow_id,omitempty"`

	// ObligationSourceType/ObligationSourceID identify the originating
	// record (e.g. "CONTRACT_CLAUSE", "FILING_RULE", "POLICY_MANDATE",
	// "JURISDICTION_RULE") in whatever service owns that source. Not a
	// foreign key — the source may live in a different service entirely.
	ObligationSourceType string `json:"obligation_source_type"`
	ObligationSourceID   string `json:"obligation_source_id"`

	// ObligationCode is a stable, human-readable identifier and the
	// idempotent creation dedup key — DATA ONLY, never a code switch/case.
	ObligationCode string `json:"obligation_code"`

	// ObligationType is a VARCHAR tag stored as data (e.g. "FILING",
	// "TAX_PAYMENT", "REGULATORY_REPORT"). New types require a data
	// migration only, never a code change.
	ObligationType string `json:"obligation_type"`

	// ObligationStatus: OPEN | IN_PROGRESS | OVERDUE | CLOSED. This one IS
	// a real state machine — see store.transitionObligationStatus for the
	// enforced legal transitions.
	ObligationStatus string `json:"obligation_status"`

	DueDate time.Time `json:"due_date"`

	// SeverityLevel is data only (e.g. "LOW", "MEDIUM", "HIGH", "CRITICAL").
	SeverityLevel string `json:"severity_level"`

	// ResponsibleFunction is a free-text/data tag identifying the owning
	// business function (e.g. "Finance", "Legal", "Tax").
	ResponsibleFunction string `json:"responsible_function"`

	// SourceReference is the human-readable Atomic Linking reference
	// (e.g. "Contract #4821 Clause 12.3", "IN-GST-FILING-RULE-07").
	// Required, never empty — see doc comment above.
	SourceReference string `json:"source_reference"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`

	// UpdatedAt is stamped on every status transition.
	UpdatedAt time.Time `json:"updated_at"`

	// ClosedAt is nil until the obligation transitions to CLOSED, set
	// exactly once, never unset or overwritten afterwards.
	ClosedAt *time.Time `json:"closed_at"`

	// Version is the optimistic locking version. Incremented on every
	// successful update. Used with If-Match header for concurrency control.
	Version int `json:"version"`
}

// FilingRequirement is a filing obligation scoped under a parent Obligation.
type FilingRequirement struct {
	FilingRequirementID string `json:"filing_requirement_id"`
	ObligationID        string `json:"obligation_id"`

	FilingType        string `json:"filing_type"`
	FilingAuthority   string `json:"filing_authority"`
	SubmissionChannel string `json:"submission_channel"`

	// FilingStatus is data only (e.g. "PENDING", "SUBMITTED", "ACCEPTED", "REJECTED").
	FilingStatus string `json:"filing_status"`

	CreatedAt time.Time `json:"created_at"`
}

// CreateObligationParams holds input parameters for creating an obligation.
type CreateObligationParams struct {
	ObligationID         string    `json:"obligation_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	JurisdictionID       string    `json:"jurisdiction_id"`
	ObligationSourceType string    `json:"obligation_source_type"`
	ObligationSourceID   string    `json:"obligation_source_id"`
	ObligationCode       string    `json:"obligation_code"`
	ObligationType       string    `json:"obligation_type"`
	DueDate              time.Time `json:"due_date"`
	SeverityLevel        string    `json:"severity_level"`
	ResponsibleFunction  string    `json:"responsible_function"`
	SourceReference      string    `json:"source_reference"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	WorkflowID           *string   `json:"workflow_id,omitempty"`
}

// ListObligationsFilter holds optional filters for querying obligations.
// Zero-value (empty string / nil) fields are not applied.
type ListObligationsFilter struct {
	// TenantID is optional on the filter and mandatory in the store, which
	// reads it from the request context. Supplying a different one here is
	// refused: a filter may narrow a read, never widen it to another tenant.
	TenantID       string
	LegalEntityID  string
	JurisdictionID string
	ObligationType string
	Status         string
	DueBefore      *time.Time
	DueAfter       *time.Time

	// Limit and Offset bound the register read. It was unbounded — every
	// obligation a tenant had ever recorded, in one response, forever.
	Limit  int
	Offset int
}

// CreateFilingRequirementParams holds input parameters for creating a
// filing requirement under an obligation.
type CreateFilingRequirementParams struct {
	FilingRequirementID string
	ObligationID        string
	FilingType          string
	FilingAuthority     string
	SubmissionChannel   string
}

// ErrObligationNotFound is returned when an obligation_id does not exist.
var ErrObligationNotFound = errorString("obligation not found")

// ErrInvalidTransition is returned when an obligation_status transition is
// illegal per the state machine (e.g. transitioning out of CLOSED).
var ErrInvalidTransition = errorString("invalid obligation status transition")

// ErrConflict is returned when an idempotent creation request matches an
// existing record's dedup key but has differing attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("obligations store unavailable")

// ErrJurisdictionNotFound is returned when JurisdictionID does not resolve
// against jurisdiction-rules-svc.
var ErrJurisdictionNotFound = errorString("jurisdiction not found")

// ErrJurisdictionServiceUnavailable is returned when jurisdiction-rules-svc
// cannot be reached to validate JurisdictionID.
var ErrJurisdictionServiceUnavailable = errorString("jurisdiction-rules-svc unavailable")

// ErrTenantMissing is returned when a request carries no X-Tenant-Id. It is an
// unscoped request, refused rather than defaulted to some shared bucket.
var ErrTenantMissing = errorString("tenant scope missing")

// ErrAuthorizationDenied is returned when authorization-svc answers DENIED.
var ErrAuthorizationDenied = errorString("not authorized for this obligation action")

// ErrAuthorizationUnavailable is returned when authorization-svc cannot be
// reached or gives an answer this service cannot read. Mutations fail closed.
var ErrAuthorizationUnavailable = errorString("authorization service unavailable")

// ErrInvalidIdentifier is what a malformed UUID becomes. obligation_id and the
// entity/jurisdiction ids are uuid columns, so a mistyped one dies inside the
// driver as SQLSTATE 22P02 before any row is examined — and used to surface as
// 503 store_unavailable, an outage status for a typo in a URL.
var ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

// ErrVersionMismatch is returned when an optimistic locking check fails
// (expected_version/If-Match header value doesn't match current version).
var ErrVersionMismatch = errorString("version mismatch: the resource has been modified by another request")

// ErrIdempotencyKeyConflict is returned when an Idempotency-Key is reused
// with a different request payload.
var ErrIdempotencyKeyConflict = errorString("idempotency key conflict: key already used with different request")

// ErrIdentityMissing is returned when a request carries no X-Principal-Id.
var ErrIdentityMissing = errorString("identity missing")

// ErrForbidden is returned when authorization check fails (403).
var ErrForbidden = errorString("forbidden")

// ErrMissingField is returned when a required field is missing from the request.
var ErrMissingField = errorString("missing field")

// ErrInvalidField is returned when a field value is invalid.
var ErrInvalidField = errorString("invalid field")

// ErrInvalidJSON is returned when the request body is not valid JSON.
var ErrInvalidJSON = errorString("invalid JSON")

// ErrUnknownField is returned when the request contains unknown fields.
var ErrUnknownField = errorString("unknown field")

// ErrRequestTooLarge is returned when the request body exceeds the size limit.
var ErrRequestTooLarge = errorString("request too large")

// ErrInvalidLimit is returned when the limit query parameter is invalid.
var ErrInvalidLimit = errorString("invalid limit")

// ErrInvalidOffset is returned when the offset query parameter is invalid.
var ErrInvalidOffset = errorString("invalid offset")

// ErrInvalidIfMatch is returned when the If-Match header is invalid.
var ErrInvalidIfMatch = errorString("invalid If-Match header")

// ErrEnvelopeIncomplete is returned when the envelope validation fails.
var ErrEnvelopeIncomplete = errorString("envelope incomplete")

// ErrWorkflowAlreadyLinked is returned when an obligation is already linked to a workflow.
var ErrWorkflowAlreadyLinked = errorString("workflow already linked")

// ErrWorkflowNotLinked is returned when an obligation is not linked to a workflow.
var ErrWorkflowNotLinked = errorString("workflow not linked")

// ErrCreatedByMismatch is returned when created_by_principal_id doesn't match the authenticated principal.
var ErrCreatedByMismatch = errorString("created_by mismatch")

// ErrLegalEntityMismatch is returned when X-Legal-Entity-Id doesn't match body legal_entity_id.
var ErrLegalEntityMismatch = errorString("legal entity mismatch")

type errorString string

func (e errorString) Error() string { return string(e) }
