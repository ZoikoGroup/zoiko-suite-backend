package domain

import (
	"errors"
	"time"
)

var (
	ErrDraftNotFound      = errors.New("filing draft not found")
	ErrDraftAlreadyFinal  = errors.New("filing draft is already finalized")
	ErrValidationBlocked  = errors.New("filing draft evidence validation failed")
	ErrMissingRequiredDoc = errors.New("missing required evidence document")
)

type ValidationStatus string

const (
	StatusDraft              ValidationStatus = "DRAFT"
	StatusValidating         ValidationStatus = "VALIDATING"
	StatusPrepared           ValidationStatus = "PREPARED"
	StatusBlocked            ValidationStatus = "BLOCKED"
	StatusReadyForSubmission ValidationStatus = "READY_FOR_SUBMISSION"
)

// FilingDraft represents a statutory filing package draft assembled prior to authority submission.
type FilingDraft struct {
	DraftID             string           `json:"draft_id"`
	TenantID            string           `json:"tenant_id"`
	LegalEntityID       string           `json:"legal_entity_id"`
	JurisdictionID      string           `json:"jurisdiction_id"`
	FilingType          string           `json:"filing_type"`
	PeriodKey           string           `json:"period_key"`
	DueDate             string           `json:"due_date"`
	PayloadData         string           `json:"payload_data"`
	EvidenceManifestRef string           `json:"evidence_manifest_ref"`
	ValidationStatus    ValidationStatus `json:"validation_status"`
	BlockReasons        string           `json:"block_reasons,omitempty"`
	Notes               string           `json:"notes,omitempty"`
	CreatedBy           string           `json:"created_by"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}

// CreateDraftRequest is the payload to assemble a new statutory filing draft.
type CreateDraftRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	JurisdictionID      string `json:"jurisdiction_id"`
	FilingType          string `json:"filing_type"`
	PeriodKey           string `json:"period_key"`
	DueDate             string `json:"due_date"`
	PayloadData         string `json:"payload_data"`
	EvidenceManifestRef string `json:"evidence_manifest_ref,omitempty"`
	Notes               string `json:"notes,omitempty"`
	CreatedBy           string `json:"created_by"`
}

// ValidateDraftRequest is the payload to trigger evidence validation on a
// draft. RequiredDocumentTypes is accepted for backward compatibility but is
// no longer authoritative — see handler.Validate's doc comment for why a
// caller-supplied "what's required" list was never a safe gate.
type ValidateDraftRequest struct {
	RequiredDocumentTypes []string `json:"required_document_types,omitempty"`
	ValidatedBy           string   `json:"validated_by"`
}

// FinalizeDraftRequest is the payload to mark a draft ready for submission.
type FinalizeDraftRequest struct {
	FinalizedBy string `json:"finalized_by"`
	Notes       string `json:"notes,omitempty"`
}

// ApplyEvidenceOutcome records the result of a real evidence-requirements-svc
// evaluation (handler.Validate) and updates draft status accordingly.
// Replaces the previous ValidateEvidence, which only checked whether a
// caller-supplied list of required document types was non-empty — a check
// the caller itself controlled, so it could always be satisfied by sending
// an empty list regardless of what the platform's actual evidence catalog
// required.
func (d *FilingDraft) ApplyEvidenceOutcome(sufficient bool, reason string) bool {
	if !sufficient {
		d.ValidationStatus = StatusBlocked
		d.BlockReasons = reason
		return false
	}
	d.ValidationStatus = StatusPrepared
	d.BlockReasons = ""
	return true
}
