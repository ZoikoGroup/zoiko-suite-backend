// Package domain defines the authoritative domain types for purchase-order-svc.
//
// Per docs/architecture/03-microservices.md §12.9, this service owns
// purchase-order issuance, amendment, and fulfillment-linked state.
// purchase-request-svc's own domain doc comment explicitly hands off to this
// service by name: an APPROVED PurchaseRequest is where a PurchaseOrder
// normally originates from (docs/architecture/04-data-model.md §14.1 marks
// PurchaseOrder.purchase_request_id nullable — a PO may also be issued
// without a prior request, e.g. for spend that doesn't require
// pre-commitment approval).
package domain

import "time"

// OrderStatus is a linear chain: ISSUED -> CLOSED. Amendment does not change
// status — it's a separate action that bumps Version and records an
// append-only PurchaseOrderAmendment row, mirroring
// employment-contracts-svc's version-lineage pattern rather than
// purchase-request-svc's fork.
type OrderStatus string

const (
	OrderStatusIssued OrderStatus = "ISSUED"
	OrderStatusClosed OrderStatus = "CLOSED"
)

// ValidOrderStatus reports whether s is a status this service can ever have
// stored. An unrecognised ?status= filter matched no row and returned an empty
// list, so a typo was indistinguishable from a tenant with no orders.
func ValidOrderStatus(s string) bool {
	switch OrderStatus(s) {
	case OrderStatusIssued, OrderStatusClosed:
		return true
	default:
		return false
	}
}

// PurchaseOrder is one order moving through issuance -> (amendments) -> close.
// Entity-bound (LegalEntityID), never hard-deleted.
type PurchaseOrder struct {
	PurchaseOrderID string      `json:"purchase_order_id"`
	TenantID        string      `json:"tenant_id"`
	LegalEntityID   string      `json:"legal_entity_id"`
	PurchaseRequestID *string   `json:"purchase_request_id,omitempty"`
	VendorProfileID   *string   `json:"vendor_profile_id,omitempty"`
	PONumber        string      `json:"po_number"`
	Status          OrderStatus `json:"po_status"`
	TotalAmount     float64     `json:"total_amount"`
	CurrencyCode    string      `json:"currency_code"`
	Version         int         `json:"version"`

	IssuedByPrincipalID string     `json:"issued_by_principal_id"`
	ClosedByPrincipalID *string    `json:"closed_by_principal_id,omitempty"`
	CorrelationID       string     `json:"correlation_id"`
	CreatedAt           time.Time  `json:"created_at"`
	IssuedAt             time.Time  `json:"issued_at"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
}

// PurchaseOrderAmendment is an append-only record of a single amendment —
// never mutated or deleted, matching doctrine's "no soft-delete, no
// destructive overwrite of material history" rule.
type PurchaseOrderAmendment struct {
	AmendmentID         string    `json:"amendment_id"`
	PurchaseOrderID     string    `json:"purchase_order_id"`
	FromVersion         int       `json:"from_version"`
	ToVersion           int       `json:"to_version"`
	PreviousTotalAmount float64   `json:"previous_total_amount"`
	NewTotalAmount      float64   `json:"new_total_amount"`
	Reason              string    `json:"reason"`
	AmendedByPrincipalID string   `json:"amended_by_principal_id"`
	AmendedAt           time.Time `json:"amended_at"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type IssueOrderRequest struct {
	TenantID          string  `json:"tenant_id"`
	LegalEntityID     string  `json:"legal_entity_id"`
	PurchaseRequestID *string `json:"purchase_request_id,omitempty"`
	VendorProfileID   *string `json:"vendor_profile_id,omitempty"`
	TotalAmount       float64 `json:"total_amount"`
	CurrencyCode      string  `json:"currency_code"`
	CorrelationID     string  `json:"correlation_id"`
}

type AmendOrderRequest struct {
	NewTotalAmount float64 `json:"new_total_amount"`
	Reason         string  `json:"reason"`
}

// ListOrdersFilter holds optional filters for querying purchase orders.
type ListOrdersFilter struct {
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
	ErrOrderNotFound     = errorString("purchase order not found")
	ErrInvalidTransition = errorString("invalid purchase order status transition")
	ErrStoreUnavailable  = errorString("purchase order store unavailable")

	// ErrInvalidIdentifier is a non-UUID value compared against a uuid column.
	// It dies inside the pg driver as SQLSTATE 22P02 before any row is
	// examined, and without this it reached the caller as a generic store
	// failure — a 503 store_unavailable, i.e. a typo wearing an outage's
	// clothes. A malformed id cannot name an existing order, so callers treat
	// this as absent rather than as the database being down.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	ErrAuthorizationDenied             = errorString("authorization denied for this purchase order action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")

	// Purchase-request verification errors — fail closed on all of these,
	// same posture as bank-reconciliation-svc's/accounts-receivable-svc's
	// general-ledger-svc verification calls (never trust a caller-supplied
	// purchase_request_id without checking it against the real record).
	ErrPurchaseRequestNotFound        = errorString("referenced purchase request not found")
	ErrPurchaseRequestNotApproved     = errorString("referenced purchase request is not APPROVED")
	ErrPurchaseRequestMismatch        = errorString("referenced purchase request belongs to a different tenant or legal entity")
	ErrPurchaseRequestServiceUnavailable = errorString("purchase-request-svc unavailable")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope (no X-Tenant-Id). Every order is tenant-owned, and a read
	// with no scope has no honest answer — it must not quietly become
	// "whatever tenant the caller named", which is what ListOrders did.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the caller's verified scope — as ?tenant_id= when listing the
	// register, or as tenant_id in an issue body. Both used to be BELIEVED in
	// preference to the header (which was never read at all), which made the
	// whole register readable, and writable, across tenants.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")
)
