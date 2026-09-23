package domain

import "time"

// BIZ-04 Form Definition & Submission — FormDefinition owns
// identity/purpose/target-domain/schema/owner; FormSubmission owns the
// actual immutable submitted values and its own lifecycle. See migration
// 000010's own comment for why the split mirrors document-vault-svc's
// Document/Version and notification-svc's TemplateDefinition/Version.
type FormDefinition struct {
	FormID                 string     `json:"form_id"`
	TenantID               string     `json:"tenant_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	Name                   string     `json:"name"`
	BusinessPurpose        string     `json:"business_purpose"`
	TargetDomain           string     `json:"target_domain"`
	Schema                 []string   `json:"schema"`
	OwnerPrincipalID       string     `json:"owner_principal_id"`
	Status                 string     `json:"status"` // DRAFT, PUBLISHED, RETIRED
	Version                int        `json:"version"`
	PublishedByPrincipalID *string    `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
	RetiredByPrincipalID   *string    `json:"retired_by_principal_id,omitempty"`
	RetiredAt              *time.Time `json:"retired_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
}

// FormSubmission status values. Draft -> Submitted -> Validating ->
// Accepted/Rejected -> Superseded, exactly as the doc's own lifecycle
// line states.
const (
	FormSubmissionDraft      = "DRAFT"
	FormSubmissionSubmitted  = "SUBMITTED"
	FormSubmissionValidating = "VALIDATING"
	FormSubmissionAccepted   = "ACCEPTED"
	FormSubmissionRejected   = "REJECTED"
	FormSubmissionSuperseded = "SUPERSEDED"
)

type FormSubmission struct {
	SubmissionID             string            `json:"submission_id"`
	FormID                   string            `json:"form_id"`
	TenantID                 string            `json:"tenant_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	FormVersion              int               `json:"form_version"`
	Status                   string            `json:"status"`
	SubmittedValues          map[string]string `json:"submitted_values"`
	SubmitterPrincipalID     string            `json:"submitter_principal_id"`
	ConsentAttestation       *string           `json:"consent_attestation,omitempty"`
	ValidationResult         *ValidationResult `json:"validation_result,omitempty"`
	RejectionReason          *string           `json:"rejection_reason,omitempty"`
	SupersededBySubmissionID *string           `json:"superseded_by_submission_id,omitempty"`
	CreatedAt                time.Time         `json:"created_at"`
	SubmittedAt              *time.Time        `json:"submitted_at,omitempty"`
	ValidatedAt              *time.Time        `json:"validated_at,omitempty"`
}

// ValidationResult is ValidateSubmission's own recorded outcome —
// GetValidationResult's query result and the value stored on the
// submission row.
type ValidationResult struct {
	Passed  bool     `json:"passed"`
	Missing []string `json:"missing,omitempty"`
}

// FormSubmissionRoute is one RouteToDomain attempt's evidence — kept
// separate from FormSubmission.Status per the doc's own failure
// semantics: a submission stays ACCEPTED forever once accepted; whether
// it was ever successfully routed, and to what, lives here.
type FormSubmissionRoute struct {
	RouteID             string    `json:"route_id"`
	SubmissionID        string    `json:"submission_id"`
	TenantID            string    `json:"tenant_id"`
	TargetDomain        string    `json:"target_domain"`
	CommandReference    *string   `json:"command_reference,omitempty"`
	ResultReference     *string   `json:"result_reference,omitempty"`
	Outcome             string    `json:"outcome"` // SUCCEEDED, FAILED
	FailureReason       *string   `json:"failure_reason,omitempty"`
	RoutedByPrincipalID string    `json:"routed_by_principal_id"`
	RoutedAt            time.Time `json:"routed_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateFormParams struct {
	TenantID, LegalEntityID, Name, BusinessPurpose, TargetDomain string
	Schema                                                       []string
	OwnerPrincipalID, CorrelationID                              string
}

type PublishFormParams struct {
	FormID, TenantID, ActorPrincipalID, CorrelationID string
}

type RetireFormParams struct {
	FormID, TenantID, ActorPrincipalID, CorrelationID string
}

// SaveDraftParams both creates a submission's first draft (SubmissionID
// empty) and updates an existing DRAFT (SubmissionID set) — mirrors how
// a real form-filling UI works: repeated partial saves before the final
// SubmitForm.
type SaveDraftParams struct {
	SubmissionID, FormID, TenantID, SubmitterPrincipalID, CorrelationID string
	SubmittedValues                                                     map[string]string
	ConsentAttestation                                                  string
}

type SubmitFormParams struct {
	SubmissionID, TenantID, ActorPrincipalID, CorrelationID string
}

type ValidateSubmissionParams struct {
	SubmissionID, TenantID, ActorPrincipalID, CorrelationID string
}

type SupersedeSubmissionParams struct {
	PreviousSubmissionID, NewSubmissionID, TenantID, ActorPrincipalID, CorrelationID string
}

type RouteToDomainParams struct {
	SubmissionID, TenantID, ActorPrincipalID, CorrelationID string
	CommandReference, ResultReference                       string
	Outcome                                                 string // SUCCEEDED, FAILED
	FailureReason                                           string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrFormNotFound                          = errorString("form not found")
	ErrFormRetired                           = errorString("form is retired; no new submissions may be created")
	ErrFormNotDraft                          = errorString("form is not in DRAFT status")
	ErrFormNotPublished                      = errorString("form is not in PUBLISHED status")
	ErrFormAlreadyRetired                    = errorString("form is already retired")
	ErrFormSelfPublish                       = errorString("the principal who owns a form cannot also publish it")
	ErrFormSubmissionNotFound                = errorString("form submission not found")
	ErrFormSubmissionNotDraft                = errorString("form submission is not in DRAFT status")
	ErrFormSubmissionNotSubmitted            = errorString("form submission is not in SUBMITTED status")
	ErrFormSubmissionNotAccepted             = errorString("form submission is not in ACCEPTED status")
	ErrFormSubmissionNotAcceptedOrRejected   = errorString("form submission is not in ACCEPTED or REJECTED status")
	ErrFormSubmissionAlreadySuperseded       = errorString("form submission has already been superseded")
	ErrFormSubmissionsBelongToDifferentForms = errorString("the two submissions belong to different forms")
)
