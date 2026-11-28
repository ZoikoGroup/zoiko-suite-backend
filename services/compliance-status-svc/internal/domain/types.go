package domain

import (
	"errors"
	"time"
)

var (
	ErrComplianceStatusNotFound = errors.New("compliance status not found")
	ErrTenantMissing            = errors.New("tenant scope missing")
	ErrVersionMismatch          = errors.New("version mismatch")
)

// ComplianceStatusCode represents the compliance status code per Doc 04 §13.1
type ComplianceStatusCode string

const (
	StatusCompliant       ComplianceStatusCode = "COMPLIANT"
	StatusNonCompliant    ComplianceStatusCode = "NON_COMPLIANT"
	StatusAtRisk          ComplianceStatusCode = "AT_RISK"
	StatusUnknown         ComplianceStatusCode = "UNKNOWN"
	StatusWaived          ComplianceStatusCode = "WAIVED"
)

// EvidenceSufficiencyState represents the evidence sufficiency state per Doc 04 §13.1
type EvidenceSufficiencyState string

const (
	EvidenceSufficient       EvidenceSufficiencyState = "SUFFICIENT"
	EvidenceInsufficient     EvidenceSufficiencyState = "INSUFFICIENT"
	EvidencePending          EvidenceSufficiencyState = "PENDING"
	EvidenceNotApplicable    EvidenceSufficiencyState = "NOT_APPLICABLE"
)

// DomainCode represents the domain code per Doc 04 §13.1
type DomainCode string

const (
	DomainTax       DomainCode = "TAX"
	DomainPayroll   DomainCode = "PAYROLL"
	DomainHR        DomainCode = "HR"
	DomainFinance   DomainCode = "FINANCE"
	DomainLegal     DomainCode = "LEGAL"
	DomainCommercial DomainCode = "COMMERCIAL"
)

// ComplianceStatus represents the compliance status per Doc 04 §13.1
type ComplianceStatus struct {
	ComplianceStatusID       string                 `json:"compliance_status_id"`
	TenantID                 string                 `json:"tenant_id"`
	LegalEntityID            string                 `json:"legal_entity_id"`
	DomainCode               DomainCode             `json:"domain_code"`
	StatusCode               ComplianceStatusCode   `json:"status_code"`
	StatusReason             string                 `json:"status_reason"`
	EvaluatedAt              time.Time              `json:"evaluated_at"`
	EvidenceSufficiencyState EvidenceSufficiencyState `json:"evidence_sufficiency_state"`
	CreatedAt                time.Time              `json:"created_at"`
	CreatedByPrincipalID     string                 `json:"created_by_principal_id"`
	UpdatedAt                time.Time              `json:"updated_at"`
	Version                  int                    `json:"version"`
}

// CreateComplianceStatusParams holds input parameters for creating a compliance status
type CreateComplianceStatusParams struct {
	ComplianceStatusID       string
	TenantID                 string
	LegalEntityID            string
	DomainCode               DomainCode
	StatusCode               ComplianceStatusCode
	StatusReason             string
	EvidenceSufficiencyState EvidenceSufficiencyState
	EvaluatedAt              time.Time
	CreatedByPrincipalID     string
	CorrelationID            string
}

// UpdateComplianceStatusParams holds input parameters for updating a compliance status
type UpdateComplianceStatusParams struct {
	StatusCode               ComplianceStatusCode
	StatusReason             string
	EvidenceSufficiencyState EvidenceSufficiencyState
	ExpectedVersion          int
}

// ListComplianceStatusFilter holds optional filters for querying compliance statuses
type ListComplianceStatusFilter struct {
	TenantID       string
	LegalEntityID  string
	DomainCode     DomainCode
	StatusCode     ComplianceStatusCode
	Limit          int
	Offset         int
}

// IsValidStatusCode checks if the status code is valid
func IsValidStatusCode(code ComplianceStatusCode) bool {
	switch code {
	case StatusCompliant, StatusNonCompliant, StatusAtRisk, StatusUnknown, StatusWaived:
		return true
	default:
		return false
	}
}

// IsValidEvidenceState checks if the evidence sufficiency state is valid
func IsValidEvidenceState(state EvidenceSufficiencyState) bool {
	switch state {
	case EvidenceSufficient, EvidenceInsufficient, EvidencePending, EvidenceNotApplicable:
		return true
	default:
		return false
	}
}

// IsValidDomainCode checks if the domain code is valid
func IsValidDomainCode(code DomainCode) bool {
	switch code {
	case DomainTax, DomainPayroll, DomainHR, DomainFinance, DomainLegal, DomainCommercial:
		return true
	default:
		return false
	}
}