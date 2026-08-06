package domain

import "time"

type VendorDDCheck struct {
	CheckID                string     `json:"check_id"`
	TenantID               string     `json:"tenant_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	CounterpartyID         string     `json:"counterparty_id"`
	VendorName             string     `json:"vendor_name"`
	Status                 string     `json:"status"`                 // STARTED, COMPLETED, FAILED
	RiskOutcome            string     `json:"risk_outcome,omitempty"` // CLEAR, FLAGGED
	ScreeningBasis         string     `json:"screening_basis,omitempty"`
	CorrelationID          string     `json:"correlation_id,omitempty"`
	InitiatedByPrincipalID string     `json:"initiated_by_principal_id"`
	StartedAt              time.Time  `json:"started_at"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

type VendorDDEvidence struct {
	EvidenceID        string    `json:"evidence_id"`
	CheckID           string    `json:"check_id"`
	TenantID          string    `json:"tenant_id"`
	EvidenceType      string    `json:"evidence_type"`
	Description       string    `json:"description"`
	DocumentReference string    `json:"document_reference,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
}

type CreateCheckRequest struct {
	CounterpartyID string `json:"counterparty_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	VendorName     string `json:"vendor_name"`
	CorrelationID  string `json:"correlation_id"`
}

type CheckDetailResponse struct {
	Check    VendorDDCheck      `json:"check"`
	Evidence []VendorDDEvidence `json:"evidence"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCheckNotFound          = errorString("vendor due diligence check not found")
	ErrAuthorizationDenied    = errorString("authorization denied for vendor due diligence action")
	ErrAuthzServiceUnavailabe = errorString("authorization-svc unavailable")
	ErrIdentityMissing        = errorString("caller identity missing")
	ErrStoreUnavailable       = errorString("store unavailable")
)
