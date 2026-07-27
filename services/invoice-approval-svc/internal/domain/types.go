package domain

import "time"

type InvoiceApprovalRequest struct {
	ApprovalRequestID    string    `json:"approval_request_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	InvoiceID            string    `json:"invoice_id"`
	WorkflowInstanceID   string    `json:"workflow_instance_id"`
	InvoiceAmount        float64   `json:"invoice_amount"`
	CurrencyCode         string    `json:"currency_code"`
	Status               string    `json:"status"` // PENDING, APPROVED, REJECTED
	CurrentStep          int       `json:"current_step"`
	TotalSteps           int       `json:"total_steps"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type ApprovalDecision struct {
	ApprovalDecisionID   string    `json:"approval_decision_id"`
	TenantID             string    `json:"tenant_id"`
	ApprovalRequestID    string    `json:"approval_request_id"`
	StepNumber           int       `json:"step_number"`
	DecidedByPrincipalID string    `json:"decided_by_principal_id"`
	Decision             string    `json:"decision"` // APPROVED, REJECTED
	DecisionReason       string    `json:"decision_reason,omitempty"`
	DecidedAt            time.Time `json:"decided_at"`
}

type CreateApprovalRequest struct {
	InvoiceID     string  `json:"invoice_id"`
	LegalEntityID string  `json:"legal_entity_id"`
	InvoiceAmount float64 `json:"invoice_amount"`
	CurrencyCode  string  `json:"currency_code"`
	TotalSteps    int     `json:"total_steps"`
}

type SubmitDecisionRequest struct {
	Decision       string `json:"decision"` // APPROVED, REJECTED
	DecisionReason string `json:"decision_reason,omitempty"`
}

type ApprovalDetailResponse struct {
	Request   InvoiceApprovalRequest `json:"request"`
	Decisions []ApprovalDecision     `json:"decisions"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRequestNotFound         = errorString("invoice approval request not found")
	ErrRequestAlreadyFinalized = errorString("invoice approval request already finalized")
	ErrAPServiceUnavailable    = errorString("accounts-payable-svc unavailable")
	ErrWorkflowUnavailable     = errorString("workflow-svc unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for invoice approval action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("invoice approval store unavailable")
)