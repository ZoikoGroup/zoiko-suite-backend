package domain

import (
	"errors"
	"time"
)

var (
	ErrTerminationNotFound = errors.New("termination request not found")
	ErrAlreadyApproved     = errors.New("termination request is already approved or completed")
	ErrAlreadyTerminated   = errors.New("termination is already finalized")
	ErrNotApproved         = errors.New("termination request must be APPROVED before it can be finalized")
	ErrChecklistNotFound   = errors.New("offboarding checklist not found")
	ErrItemNotFound        = errors.New("checklist item not found")
	ErrUnauthorizedAccess  = errors.New("unauthorized action for offboarding governance")

	ErrAuthorizationDenied     = errors.New("authorization denied for this offboarding action")
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved tenant (no X-Tenant-Id header) — fail closed rather than
	// defaulting to a shared tenant bucket.
	ErrIdentityMissing = errors.New("tenant identity missing")
)

type TerminationType string

const (
	TerminationTypeResignation TerminationType = "RESIGNATION"
	TerminationTypeInvoluntary TerminationType = "INVOLUNTARY"
	TerminationTypeRedundancy  TerminationType = "REDUNDANCY"
	TerminationTypeRetirement  TerminationType = "RETIREMENT"
	TerminationTypeContractEnd TerminationType = "CONTRACT_END"
)

type TerminationStatus string

const (
	TerminationStatusInitiated  TerminationStatus = "INITIATED"
	TerminationStatusApproved   TerminationStatus = "APPROVED"
	TerminationStatusTerminated TerminationStatus = "TERMINATED"
	TerminationStatusCancelled  TerminationStatus = "CANCELLED"
)

type ChecklistItemStatus string

const (
	ChecklistItemStatusPending    ChecklistItemStatus = "PENDING"
	ChecklistItemStatusInProgress ChecklistItemStatus = "IN_PROGRESS"
	ChecklistItemStatusCompleted  ChecklistItemStatus = "COMPLETED"
	ChecklistItemStatusWaived     ChecklistItemStatus = "WAIVED"
)

type TerminationRequest struct {
	TerminationID    string            `json:"termination_id"`
	TenantID         string            `json:"tenant_id"`
	LegalEntityID    string            `json:"legal_entity_id"`
	EmployeeID       string            `json:"employee_id"`
	TerminationType  TerminationType   `json:"termination_type"`
	ReasonCode       string            `json:"reason_code"`
	ReasonDetails    string            `json:"reason_details,omitempty"`
	NoticePeriodDays int               `json:"notice_period_days"`
	LastWorkingDay   string            `json:"last_working_day"`
	EffectiveFrom    string            `json:"effective_from"`
	EffectiveTo      *string           `json:"effective_to,omitempty"`
	Status           TerminationStatus `json:"status"`
	InitiatedBy      string            `json:"initiated_by"`
	ApprovedBy       *string           `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time        `json:"approved_at,omitempty"`
	SeveranceAmount  float64           `json:"severance_amount"`
	Currency         string            `json:"currency"`
	CorrelationID    string            `json:"correlation_id"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type OffboardingChecklist struct {
	ChecklistID   string          `json:"checklist_id"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	EmployeeID    string          `json:"employee_id"`
	TerminationID string          `json:"termination_id"`
	Status        string          `json:"status"`
	CorrelationID string          `json:"correlation_id"`
	Items         []ChecklistItem `json:"items"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type ChecklistItem struct {
	ItemID      string              `json:"item_id"`
	ChecklistID string              `json:"checklist_id"`
	Category    string              `json:"category"` // ASSET_RETURN, ACCESS_REVOCATION, FINAL_PAY_AUDIT, EXIT_INTERVIEW
	Description string              `json:"description"`
	Status      ChecklistItemStatus `json:"status"`
	CompletedBy *string             `json:"completed_by,omitempty"`
	CompletedAt *time.Time          `json:"completed_at,omitempty"`
}

type InitiateTerminationRequest struct {
	LegalEntityID    string          `json:"legal_entity_id"`
	EmployeeID       string          `json:"employee_id"`
	TerminationType  TerminationType `json:"termination_type"`
	ReasonCode       string          `json:"reason_code"`
	ReasonDetails    string          `json:"reason_details,omitempty"`
	NoticePeriodDays int             `json:"notice_period_days"`
	LastWorkingDay   string          `json:"last_working_day"`
	EffectiveFrom    string          `json:"effective_from"`
	SeveranceAmount  float64         `json:"severance_amount"`
	Currency         string          `json:"currency"`
	CorrelationID    string          `json:"correlation_id"`
}

type ApproveTerminationRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type CreateChecklistRequest struct {
	LegalEntityID string          `json:"legal_entity_id"`
	EmployeeID    string          `json:"employee_id"`
	TerminationID string          `json:"termination_id"`
	CorrelationID string          `json:"correlation_id"`
	Items         []ChecklistItem `json:"items"`
}

type UpdateChecklistItemRequest struct {
	Status      ChecklistItemStatus `json:"status"`
	CompletedBy string              `json:"completed_by"`
}
