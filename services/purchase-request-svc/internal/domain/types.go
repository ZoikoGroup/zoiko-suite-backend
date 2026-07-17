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

	ErrAuthorizationDenied             = errorString("authorization denied for this purchase request action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")
)
