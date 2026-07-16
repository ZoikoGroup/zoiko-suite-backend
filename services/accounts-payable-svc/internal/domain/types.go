// Package domain defines the authoritative domain types for accounts-payable-svc.
//
// Per docs/architecture/03-microservices.md §10.3, this service owns vendor
// invoice intake, liability-side invoice lifecycle, and payment readiness
// state. It does NOT own a vendor master: no Vendor Master service exists
// yet anywhere in this platform, so vendor_id is a plain caller-supplied
// string reference, unvalidated — same documented v1 gap as
// general-ledger-svc's account_code (no Chart-of-Accounts service either).
package domain

import "time"

// InvoiceStatus is the liability-side lifecycle: RECEIVED -> VALIDATED ->
// APPROVED -> PAYMENT_REQUESTED. Critical constraint (spec): "No payable may
// proceed to payment initiation without approval-state and evidence-state
// validation" — enforced here by making PAYMENT_REQUESTED reachable only
// from APPROVED, itself only reachable from VALIDATED, itself only from
// RECEIVED. There is no way to skip a state; the sequential transition
// itself IS the evidence that every prior check actually happened.
// PAYMENT_REQUESTED is terminal for this service — actual payment execution
// belongs to a future Treasury/Payments service, out of scope here.
type InvoiceStatus string

const (
	InvoiceStatusReceived        InvoiceStatus = "RECEIVED"
	InvoiceStatusValidated       InvoiceStatus = "VALIDATED"
	InvoiceStatusApproved        InvoiceStatus = "APPROVED"
	InvoiceStatusPaymentRequested InvoiceStatus = "PAYMENT_REQUESTED"
)

// ValidInvoiceTransitions enumerates the only legal status transitions.
var ValidInvoiceTransitions = map[InvoiceStatus][]InvoiceStatus{
	InvoiceStatusReceived:         {InvoiceStatusValidated},
	InvoiceStatusValidated:        {InvoiceStatusApproved},
	InvoiceStatusApproved:         {InvoiceStatusPaymentRequested},
	InvoiceStatusPaymentRequested: {},
}

// VendorInvoice is one vendor invoice moving through the liability-side
// lifecycle. Entity-bound (LegalEntityID), never hard-deleted.
type VendorInvoice struct {
	InvoiceID     string        `json:"invoice_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	VendorID      string        `json:"vendor_id"`
	InvoiceNumber string        `json:"invoice_number"`
	Amount        float64       `json:"amount"`
	CurrencyCode  string        `json:"currency_code"`
	DueDate       time.Time     `json:"due_date"`
	Status        InvoiceStatus `json:"status"`

	CreatedByPrincipalID       string     `json:"created_by_principal_id"`
	ValidatedByPrincipalID     *string    `json:"validated_by_principal_id,omitempty"`
	ApprovedByPrincipalID      *string    `json:"approved_by_principal_id,omitempty"`
	PaymentRequestedByPrincipalID *string `json:"payment_requested_by_principal_id,omitempty"`
	CorrelationID              string     `json:"correlation_id"`
	CreatedAt                  time.Time  `json:"created_at"`
	ValidatedAt                *time.Time `json:"validated_at,omitempty"`
	ApprovedAt                 *time.Time `json:"approved_at,omitempty"`
	PaymentRequestedAt         *time.Time `json:"payment_requested_at,omitempty"`
}

// ── wire types (request bodies) ─────────────────────────────────────────────

type CreateVendorInvoiceRequest struct {
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	VendorID      string    `json:"vendor_id"`
	InvoiceNumber string    `json:"invoice_number"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	DueDate       time.Time `json:"due_date"`
	CorrelationID string    `json:"correlation_id"`
}

// ListInvoicesFilter holds optional filters for querying invoices.
type ListInvoicesFilter struct {
	TenantID      string
	LegalEntityID string
	VendorID      string
	Status        string
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrInvoiceNotFound   = errorString("vendor invoice not found")
	ErrInvalidTransition = errorString("invalid invoice status transition")
	ErrStoreUnavailable  = errorString("accounts payable store unavailable")

	ErrAuthorizationDenied             = errorString("authorization denied for this invoice action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as general-ledger-svc/schema-registry-svc.
	ErrIdentityMissing = errorString("caller identity missing")
)
