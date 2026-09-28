package privacy

import (
	"time"

	"zoiko.io/contract/types"
)

// PrivacyRequestType represents GDPR/CCPA data subject requests (§18, §28).
type PrivacyRequestType string

const (
	RequestTypeErasure     PrivacyRequestType = "ERASURE"     // Right to be forgotten / deletion
	RequestTypeRestriction PrivacyRequestType = "RESTRICTION" // Restriction of processing
	RequestTypeRectify     PrivacyRequestType = "RECTIFY"
)

// ResolutionOutcome specifies the 5 permitted policy resolution outcomes (§18.1, DG-040, DG-052).
type ResolutionOutcome string

const (
	OutcomeErase         ResolutionOutcome = "ERASE"           // Full erasure lawful
	OutcomeAnonymize     ResolutionOutcome = "ANONYMIZE"       // Anonymization / pseudonymization
	OutcomePartial       ResolutionOutcome = "PARTIAL"         // Mixed retained/nonretained fields (NP-18)
	OutcomeDefer         ResolutionOutcome = "DEFER"           // Active legal hold or claim blocks deletion (NP-17)
	OutcomeDenyWithBasis ResolutionOutcome = "DENY_WITH_BASIS" // Lawful statutory requirement denies request
)

// PrivacyDispositionRequest models a governed privacy erasure/restriction case (§18, §28).
type PrivacyDispositionRequest struct {
	RequestID     types.UUID         `json:"request_id"`
	TenantID      types.UUID         `json:"tenant_id"`
	DataSubjectID string             `json:"data_subject_id"`
	RequestType   PrivacyRequestType `json:"request_type"`
	RequestedAt   time.Time          `json:"requested_at"`
	Status        string             `json:"status"` // "PENDING", "RESOLVED", "REJECTED"
}

// FieldResolutionDetail records granular field/derivative disposition actions for PARTIAL outcomes (§18.1, NP-18).
type FieldResolutionDetail struct {
	FieldName            string `json:"field_name"`
	Action               string `json:"action"` // "ERASE", "RETAIN", "ANONYMIZE"
	LegalRegulatoryBasis string `json:"legal_regulatory_basis,omitempty"`
}

// PrivacyResolution records the policy resolution outcome and executable actions (§18, §28, DG-040, DG-052).
type PrivacyResolution struct {
	ResolutionID         types.UUID              `json:"resolution_id"`
	TenantID             types.UUID              `json:"tenant_id"`
	RequestID            types.UUID              `json:"request_id"`
	Outcome              ResolutionOutcome       `json:"outcome"`
	LegalRegulatoryBasis string                  `json:"legal_regulatory_basis"`
	HoldBlockRef         *string                 `json:"hold_block_ref,omitempty"` // Reference to legal hold if DEFER
	FieldActions         []FieldResolutionDetail `json:"field_actions,omitempty"`
	ReviewDate           *time.Time              `json:"review_date,omitempty"`
	ResolvedAt           time.Time               `json:"resolved_at"`
	ResolvedBy           string                  `json:"resolved_by"`
}
