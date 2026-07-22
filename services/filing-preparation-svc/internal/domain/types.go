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
	DraftID              string           `json:"draft_id"`
	TenantID             string           `json:"tenant_id"`
	LegalEntityID        string           `json:"legal_entity_id"`
	JurisdictionID       string           `json:"jurisdiction_id"`
	FilingType           string           `json:"filing_type"`
	PeriodKey            string           `json:"period_key"`
	DueDate              string           `json:"due_date"`
	PayloadData          string           `json:"payload_data"`
	EvidenceManifestRef  string           `json:"evidence_manifest_ref"`
	ValidationStatus     ValidationStatus `json:"validation_status"`
	BlockReasons         string           `json:"block_reasons,omitempty"`
	Notes                string           `json:"notes,omitempty"`
	CreatedBy            string           `json:"created_by"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
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

// ValidateDraftRequest is the payload to trigger evidence validation on a draft.
type ValidateDraftRequest struct {
	RequiredDocumentTypes []string `json:"required_document_types,omitempty"`
	ValidatedBy           string   `json:"validated_by"`
}

// FinalizeDraftRequest is the payload to mark a draft ready for submission.
type FinalizeDraftRequest struct {
	FinalizedBy string `json:"finalized_by"`
	Notes       string `json:"notes,omitempty"`
}

// ValidateEvidence checks evidence completeness and updates draft status.
func (d *FilingDraft) ValidateEvidence(requiredDocs []string) bool {
	if d.EvidenceManifestRef == "" && len(requiredDocs) > 0 {
		d.ValidationStatus = StatusBlocked
		d.BlockReasons = "Evidence manifest reference is missing while required documents are specified."
		return false
	}
	d.ValidationStatus = StatusPrepared
	d.BlockReasons = ""
	return true
}
