// Package domain defines the authoritative domain types for purchase-request-svc.
//
// Per docs/architecture/03-microservices.md §12.8 and ZS-SVC-D-001 §5 (AP-02),
// this service owns the purchase requisition and its lifecycle before supplier
// commitment: Draft -> PendingApproval -> Approved/Rejected ->
// Converted/Cancelled/Expired. It is non-posting. It does NOT own purchase
// orders (purchase-order-svc, AP-03): ConvertToPurchaseOrder asks that service
// to create the PO and records the link, and an approved requisition cannot
// change before it is converted — any amendment invalidates the approval.
package domain

import (
	"math"
	"time"
)

// RequestStatus is the requisition lifecycle (spec §5 state model).
type RequestStatus string

const (
	RequestStatusDraft     RequestStatus = "DRAFT"
	RequestStatusPending   RequestStatus = "PENDING_APPROVAL"
	RequestStatusApproved  RequestStatus = "APPROVED"
	RequestStatusRejected  RequestStatus = "REJECTED"
	RequestStatusConverted RequestStatus = "CONVERTED"
	RequestStatusCancelled RequestStatus = "CANCELLED"
	RequestStatusExpired   RequestStatus = "EXPIRED"
)

// NormalizeStatus maps the pre-lifecycle spelling "PENDING" (rows and filters
// written before the Draft state existed) onto PENDING_APPROVAL.
func NormalizeStatus(s string) RequestStatus {
	if s == "PENDING" {
		return RequestStatusPending
	}
	return RequestStatus(s)
}

// ValidRequestStatus reports whether s is a status this service can ever have
// stored. An unrecognised ?status= filter matched no row and returned an empty
// list, so a typo was indistinguishable from a tenant with no requests.
func ValidRequestStatus(s string) bool {
	switch NormalizeStatus(s) {
	case RequestStatusDraft, RequestStatusPending, RequestStatusApproved, RequestStatusRejected,
		RequestStatusConverted, RequestStatusCancelled, RequestStatusExpired:
		return true
	default:
		return false
	}
}

// ValidRequestTransitions enumerates the only legal status transitions. The
// same table is enforced in the database by trg_guard_purchase_request.
var ValidRequestTransitions = map[RequestStatus][]RequestStatus{
	RequestStatusDraft:     {RequestStatusPending, RequestStatusCancelled},
	RequestStatusPending:   {RequestStatusApproved, RequestStatusRejected, RequestStatusDraft, RequestStatusCancelled, RequestStatusExpired},
	RequestStatusApproved:  {RequestStatusConverted, RequestStatusDraft, RequestStatusCancelled, RequestStatusExpired},
	RequestStatusRejected:  {},
	RequestStatusConverted: {},
	RequestStatusCancelled: {},
	RequestStatusExpired:   {},
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to RequestStatus) bool {
	for _, t := range ValidRequestTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// BudgetDecision is the recorded outcome of the budget/policy check made at
// submission. APPROVE refuses a controlled requisition whose decision is not
// ALLOWED, so a bypassed check cannot be approved around.
type BudgetDecision string

const (
	BudgetNotChecked  BudgetDecision = "NOT_CHECKED"
	BudgetNotRequired BudgetDecision = "NOT_REQUIRED"
	BudgetAllowed     BudgetDecision = "ALLOWED"
	BudgetBlocked     BudgetDecision = "BLOCKED"
)

// RequestLine is one requisition line (spec §5 required inputs).
type RequestLine struct {
	LineID               string   `json:"line_id"`
	LineNumber           int      `json:"line_number"`
	ItemRef              string   `json:"item_ref"`
	Description          string   `json:"description"`
	Category             string   `json:"category"`
	Quantity             float64  `json:"quantity"`
	UnitOfMeasure        string   `json:"unit_of_measure,omitempty"`
	Amount               float64  `json:"amount"`
	CurrencyCode         string   `json:"currency_code"`
	RequiredDate         *string  `json:"required_date,omitempty"` // YYYY-MM-DD
	CostCenter           string   `json:"cost_center,omitempty"`
	ProjectRef           string   `json:"project_ref,omitempty"`
	BudgetRef            string   `json:"budget_ref,omitempty"`
	PreferredSupplierRef string   `json:"preferred_supplier_ref,omitempty"`
	AttachmentRefs       []string `json:"attachment_refs"`
}

// PurchaseRequest is one requisition. Entity-bound (LegalEntityID), never
// hard-deleted. Amount is always the sum of its lines (kept as a header field
// for the original wire shape).
type PurchaseRequest struct {
	RequestID              string        `json:"request_id"`
	TenantID               string        `json:"tenant_id"`
	LegalEntityID          string        `json:"legal_entity_id"`
	RequestedByPrincipalID string        `json:"requested_by_principal_id"`
	Description            string        `json:"description"`
	Amount                 float64       `json:"amount"`
	CurrencyCode           string        `json:"currency_code"`
	Status                 RequestStatus `json:"status"`
	Version                int           `json:"version"`

	BusinessPurpose      string        `json:"business_purpose"`
	CostCenter           string        `json:"cost_center"`
	ProjectRef           string        `json:"project_ref"`
	BudgetRef            string        `json:"budget_ref"`
	PreferredSupplierRef string        `json:"preferred_supplier_ref"`
	RequiredDate         *string       `json:"required_date,omitempty"`
	AttachmentRefs       []string      `json:"attachment_refs"`
	ExpiresAt            *time.Time    `json:"expires_at,omitempty"`
	Lines                []RequestLine `json:"lines"`

	BudgetDecision BudgetDecision `json:"budget_decision"`
	BudgetBasis    string         `json:"budget_basis"`

	SubmittedByPrincipalID   *string `json:"submitted_by_principal_id,omitempty"`
	LastAmendedByPrincipalID *string `json:"last_amended_by_principal_id,omitempty"`
	ApprovedByPrincipalID    *string `json:"approved_by_principal_id,omitempty"`
	RejectedByPrincipalID    *string `json:"rejected_by_principal_id,omitempty"`
	RejectionReason          *string `json:"rejection_reason,omitempty"`
	CancelledByPrincipalID   *string `json:"cancelled_by_principal_id,omitempty"`
	CancellationReason       *string `json:"cancellation_reason,omitempty"`
	ApprovalInvalidatedCount int     `json:"approval_invalidated_count"`
	ConvertedPurchaseOrderID *string `json:"converted_purchase_order_id,omitempty"`
	ConvertedByPrincipalID   *string `json:"converted_by_principal_id,omitempty"`

	CorrelationID string     `json:"correlation_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
	RejectedAt    *time.Time `json:"rejected_at,omitempty"`
	CancelledAt   *time.Time `json:"cancelled_at,omitempty"`
	ConvertedAt   *time.Time `json:"converted_at,omitempty"`
}

// RecomputeAmount sets Amount to the (2dp) sum of the lines.
func (r *PurchaseRequest) RecomputeAmount() {
	var sum float64
	for _, l := range r.Lines {
		sum += l.Amount
	}
	r.Amount = Round2(sum)
}

// Round2 rounds to 2 decimal places (money).
func Round2(v float64) float64 { return math.Round(v*100) / 100 }

// HistoryEntry is one append-only row of a requisition's history.
type HistoryEntry struct {
	HistoryID  string         `json:"history_id"`
	RequestID  string         `json:"request_id"`
	Version    int            `json:"version"`
	Action     string         `json:"action"`
	FromStatus *string        `json:"from_status,omitempty"`
	ToStatus   string         `json:"to_status"`
	Actor      string         `json:"actor"`
	Reason     string         `json:"reason,omitempty"`
	Details    map[string]any `json:"details"`
	CreatedAt  time.Time      `json:"created_at"`
}

// ApprovalStatus is the GetApprovalStatus read model.
type ApprovalStatus struct {
	RequestID                 string         `json:"request_id"`
	Status                    RequestStatus  `json:"status"`
	Version                   int            `json:"version"`
	Amount                    float64        `json:"amount"`
	CurrencyCode              string         `json:"currency_code"`
	ApprovalThreshold         float64        `json:"approval_threshold"`
	AboveThreshold            bool           `json:"above_threshold"`
	IndependentApproverNeeded bool           `json:"independent_approver_required"`
	BudgetDecision            BudgetDecision `json:"budget_decision"`
	ApprovedBy                *string        `json:"approved_by_principal_id,omitempty"`
	ApprovedAt                *time.Time     `json:"approved_at,omitempty"`
	RejectedBy                *string        `json:"rejected_by_principal_id,omitempty"`
	ApprovalInvalidatedCount  int            `json:"approval_invalidated_count"`
	ConvertedPurchaseOrderID  *string        `json:"converted_purchase_order_id,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

// RequestLineInput is a line as submitted by a caller.
type RequestLineInput struct {
	ItemRef              string   `json:"item_ref"`
	Description          string   `json:"description"`
	Category             string   `json:"category"`
	Quantity             float64  `json:"quantity"`
	UnitOfMeasure        string   `json:"unit_of_measure"`
	Amount               float64  `json:"amount"`
	CurrencyCode         string   `json:"currency_code"`
	RequiredDate         string   `json:"required_date"`
	CostCenter           string   `json:"cost_center"`
	ProjectRef           string   `json:"project_ref"`
	BudgetRef            string   `json:"budget_ref"`
	PreferredSupplierRef string   `json:"preferred_supplier_ref"`
	AttachmentRefs       []string `json:"attachment_refs"`
}

type CreateRequestRequest struct {
	TenantID             string             `json:"tenant_id"`
	LegalEntityID        string             `json:"legal_entity_id"`
	Description          string             `json:"description"`
	Amount               float64            `json:"amount"` // legacy header-only form; ignored when lines are supplied
	CurrencyCode         string             `json:"currency_code"`
	CorrelationID        string             `json:"correlation_id"`
	BusinessPurpose      string             `json:"business_purpose"`
	CostCenter           string             `json:"cost_center"`
	ProjectRef           string             `json:"project_ref"`
	BudgetRef            string             `json:"budget_ref"`
	PreferredSupplierRef string             `json:"preferred_supplier_ref"`
	RequiredDate         string             `json:"required_date"`
	AttachmentRefs       []string           `json:"attachment_refs"`
	ExpiresAt            *time.Time         `json:"expires_at"`
	Lines                []RequestLineInput `json:"lines"`
}

// AmendRequestRequest carries only the fields being changed; Lines, when
// present, REPLACES the whole line set.
type AmendRequestRequest struct {
	ExpectedVersion      *int                `json:"expected_version"`
	Description          *string             `json:"description"`
	BusinessPurpose      *string             `json:"business_purpose"`
	CostCenter           *string             `json:"cost_center"`
	ProjectRef           *string             `json:"project_ref"`
	BudgetRef            *string             `json:"budget_ref"`
	PreferredSupplierRef *string             `json:"preferred_supplier_ref"`
	RequiredDate         *string             `json:"required_date"`
	AttachmentRefs       *[]string           `json:"attachment_refs"`
	ExpiresAt            *time.Time          `json:"expires_at"`
	Lines                *[]RequestLineInput `json:"lines"`
	Reason               string              `json:"reason"`
}

type VersionedRequest struct {
	ExpectedVersion *int `json:"expected_version"`
}

type RejectRequestRequest struct {
	Reason          string `json:"reason"`
	CorrelationID   string `json:"correlation_id"`
	ExpectedVersion *int   `json:"expected_version"`
}

type CancelRequestRequest struct {
	Reason          string `json:"reason"`
	ExpectedVersion *int   `json:"expected_version"`
}

// ConvertRequestRequest asks for the requisition to be turned into a PO.
type ConvertRequestRequest struct {
	ExpectedVersion *int `json:"expected_version"`
	// SupplierRef/VendorProfileID name the supplier the PO is placed with; when
	// absent the requisition's preferred_supplier_ref is used.
	SupplierRef     string `json:"supplier_ref"`
	VendorProfileID string `json:"vendor_profile_id"`
}

// ListRequestsFilter holds optional filters for querying purchase requests.
type ListRequestsFilter struct {
	// TenantID is the caller's VERIFIED scope, resolved from X-Tenant-Id by the
	// handler — never a value the request chose for itself.
	TenantID      string
	LegalEntityID string
	Status        string
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRequestNotFound   = errorString("purchase request not found")
	ErrInvalidTransition = errorString("invalid purchase request status transition")
	ErrStoreUnavailable  = errorString("purchase request store unavailable")
	ErrStaleVersion      = errorString("purchase request version changed concurrently")

	// ErrInvalidIdentifier is a non-UUID value compared against a uuid column.
	// It dies inside the pg driver as SQLSTATE 22P02 before any row is
	// examined, and without this it reached the caller as a generic store
	// failure — a 503 store_unavailable, i.e. a typo wearing an outage's
	// clothes. A malformed id cannot name an existing request, so callers
	// treat this as absent rather than as the database being down.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	ErrAuthorizationDenied             = errorString("authorization denied for this purchase request action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrSelfApprovalNotAllowed enforces the platform's Segregation of Duties
	// doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3): the principal
	// who created a record may not be the same principal who approves or
	// rejects it.
	ErrSelfApprovalNotAllowed = errorString("principal may not approve or reject their own submission")

	// ErrIndependentApproverRequired: above the approval threshold the approver
	// must be independent of every principal who made or submitted the request.
	ErrIndependentApproverRequired = errorString("requisition above the approval threshold needs an approver independent of its requester, last amender and submitter")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope (no X-Tenant-Id). Every row here is tenant-owned, and a
	// read with no scope has no honest answer — it must not quietly become
	// "whatever tenant the caller named", which is what ListRequests did.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the caller's verified scope — as ?tenant_id= when listing the
	// register, or as tenant_id in a create body. Both used to be BELIEVED in
	// preference to the header (which was never read at all), which made the
	// whole register readable, and writable, across tenants.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")

	// Budget / policy check (spec §5 failure semantics).
	ErrBudgetBlocked     = errorString("budget or spend policy blocked this requisition")
	ErrBudgetUnavailable = errorString("budget/policy service unavailable — controlled categories cannot be submitted")
	ErrBudgetNotAllowed  = errorString("requisition has no ALLOWED budget decision for its controlled categories")

	// Conversion.
	ErrNotConvertible           = errorString("only an APPROVED requisition can be converted to a purchase order")
	ErrPurchaseOrderUnavailable = errorString("purchase-order-svc unavailable")
	ErrPurchaseOrderRefused     = errorString("purchase-order-svc refused the conversion")
	ErrSupplierRequired         = errorString("a supplier reference is required to convert a requisition")
	ErrApprovedRequestImmutable = errorString("an approved requisition cannot change before conversion; amend it to drop it back to DRAFT")
)
