// Package domain defines the authoritative domain types for purchase-request-svc.
//
// Per docs/architecture/03-microservices.md §12.8, this service owns
// purchase-request objects and their lifecycle before order issuance. It
// does NOT own purchase orders — that's a separate, not-yet-built service
// (Purchase Order Service) this one hands off to via events.
package domain

import "time"

// RequestStatus is a fork, not a chain: PENDING splits into either APPROVED
// or REJECTED, and both are terminal — unlike general-ledger-svc's or
// accounts-payable-svc's linear multi-stage chains.
type RequestStatus string

const (
	RequestStatusPending  RequestStatus = "PENDING"
	RequestStatusApproved RequestStatus = "APPROVED"
	RequestStatusRejected RequestStatus = "REJECTED"
)

// ValidRequestStatus reports whether s is a status this service can ever have
// stored. An unrecognised ?status= filter matched no row and returned an empty
// list, so a typo was indistinguishable from a tenant with no requests.
func ValidRequestStatus(s string) bool {
	switch RequestStatus(s) {
	case RequestStatusPending, RequestStatusApproved, RequestStatusRejected:
		return true
	default:
		return false
	}
}

// ValidRequestTransitions enumerates the only legal status transitions.
var ValidRequestTransitions = map[RequestStatus][]RequestStatus{
	RequestStatusPending:  {RequestStatusApproved, RequestStatusRejected},
	RequestStatusApproved: {},
	RequestStatusRejected: {},
}

// PurchaseRequest is one request moving through the fork lifecycle.
// Entity-bound (LegalEntityID), never hard-deleted.
type PurchaseRequest struct {
	RequestID              string        `json:"request_id"`
	TenantID                string        `json:"tenant_id"`
	LegalEntityID           string        `json:"legal_entity_id"`
	RequestedByPrincipalID  string        `json:"requested_by_principal_id"`
	Description             string        `json:"description"`
	Amount                  float64       `json:"amount"`
	CurrencyCode            string        `json:"currency_code"`
	Status                  RequestStatus `json:"status"`

	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
	RejectedByPrincipalID *string    `json:"rejected_by_principal_id,omitempty"`
	RejectionReason       *string    `json:"rejection_reason,omitempty"`
	CorrelationID         string     `json:"correlation_id"`
	CreatedAt             time.Time  `json:"created_at"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	RejectedAt            *time.Time `json:"rejected_at,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type CreateRequestRequest struct {
	TenantID      string  `json:"tenant_id"`
	LegalEntityID string  `json:"legal_entity_id"`
	Description   string  `json:"description"`
	Amount        float64 `json:"amount"`
	CurrencyCode  string  `json:"currency_code"`
	CorrelationID string  `json:"correlation_id"`
}

type RejectRequestRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id"`
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
)
