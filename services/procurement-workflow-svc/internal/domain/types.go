// Package domain defines the authoritative domain types for
// procurement-workflow-svc.
//
// Per docs/architecture/03-microservices.md §12.7, this service "owns
// procurement orchestration and governed spend routing." It does not own
// purchase requests or purchase orders — purchase-request-svc and
// purchase-order-svc already own those. This service's job is the saga in
// between: given a procurement need, it runs a real spend-controls check
// (calling spend-controls-svc synchronously), routes the case through
// approval, and — once approved — issues the real purchase order (calling
// purchase-order-svc synchronously), matching the doctrine's saga-discipline
// note in §17.8 that Procure-to-Pay is a multi-service flow.
package domain

import "time"

// CaseStatus is a linear-with-one-fork chain:
//
//	REQUESTED -> SPEND_BLOCKED (terminal, spend-controls denied the amount)
//	REQUESTED -> APPROVAL_PENDING -> APPROVED -> COMPLETED
//	                               -> REJECTED (terminal)
type CaseStatus string

const (
	CaseStatusRequested       CaseStatus = "REQUESTED"
	CaseStatusSpendBlocked    CaseStatus = "SPEND_BLOCKED"
	CaseStatusApprovalPending CaseStatus = "APPROVAL_PENDING"
	CaseStatusApproved        CaseStatus = "APPROVED"
	CaseStatusRejected        CaseStatus = "REJECTED"
	CaseStatusCompleted       CaseStatus = "COMPLETED"
)

// ValidCaseTransitions enumerates the only legal status transitions.
var ValidCaseTransitions = map[CaseStatus][]CaseStatus{
	CaseStatusRequested:       {CaseStatusSpendBlocked, CaseStatusApprovalPending},
	CaseStatusSpendBlocked:    {},
	CaseStatusApprovalPending: {CaseStatusApproved, CaseStatusRejected},
	CaseStatusApproved:        {CaseStatusCompleted},
	CaseStatusRejected:        {},
	CaseStatusCompleted:       {},
}

// ProcurementCase is one procurement need moving through spend-check ->
// approval -> order issuance. Entity-bound (LegalEntityID), never
// hard-deleted.
type ProcurementCase struct {
	CaseID                 string     `json:"case_id"`
	TenantID               string     `json:"tenant_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	Description            string     `json:"description"`
	Category               string     `json:"category"`
	Amount                 float64    `json:"amount"`
	CurrencyCode           string     `json:"currency_code"`
	VendorProfileID        *string    `json:"vendor_profile_id,omitempty"`
	Status                 CaseStatus `json:"status"`
	SpendCheckDecision     string     `json:"spend_check_decision,omitempty"` // ALLOWED, BLOCKED
	SpendCheckBasis        string     `json:"spend_check_basis,omitempty"`
	PurchaseOrderID        *string    `json:"purchase_order_id,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	RejectedByPrincipalID  *string    `json:"rejected_by_principal_id,omitempty"`
	RejectionReason        *string    `json:"rejection_reason,omitempty"`
	CorrelationID          string     `json:"correlation_id"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	RejectedAt             *time.Time `json:"rejected_at,omitempty"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type CreateCaseRequest struct {
	LegalEntityID   string  `json:"legal_entity_id"`
	Description     string  `json:"description"`
	Category        string  `json:"category"`
	Amount          float64 `json:"amount"`
	CurrencyCode    string  `json:"currency_code"`
	VendorProfileID *string `json:"vendor_profile_id,omitempty"`
	CorrelationID   string  `json:"correlation_id"`
}

type RejectCaseRequest struct {
	Reason string `json:"reason"`
}

type ListCasesFilter struct {
	LegalEntityID string
	Status        string
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCaseNotFound      = errorString("procurement case not found")
	ErrInvalidTransition = errorString("invalid procurement case status transition")
	ErrStoreUnavailable  = errorString("procurement workflow store unavailable")

	ErrAuthorizationDenied     = errorString("authorization denied for this procurement action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrSpendControlsUnavailable is returned when spend-controls-svc cannot
	// be reached. A procurement case must never be created without a real
	// spend-check outcome — failing open here would defeat the entire
	// purpose of routing spend through this service.
	ErrSpendControlsUnavailable = errorString("spend-controls-svc unavailable")

	// ErrPurchaseOrderServiceUnavailable is returned when purchase-order-svc
	// cannot be reached during order issuance. The case is left APPROVED,
	// not COMPLETED, so issuance can be safely retried.
	ErrPurchaseOrderServiceUnavailable = errorString("purchase-order-svc unavailable")

	// ErrSelfApprovalNotAllowed enforces the platform's Segregation of Duties
	// doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3): the principal
	// who created a record may not be the same principal who approves or
	// rejects it.
	ErrSelfApprovalNotAllowed = errorString("principal may not approve or reject their own submission")
)
