// Package domain defines the authoritative domain types for accounts-payable-svc.
//
// Per docs/architecture/03-microservices.md §10.3, this service owns vendor
// invoice intake, liability-side invoice lifecycle, and payment readiness
// state. It does NOT own a vendor master: no Vendor Master service exists
// yet anywhere in this platform, so vendor_id is a plain caller-supplied
// string reference, unvalidated — same documented v1 gap as
// general-ledger-svc's account_code (no Chart-of-Accounts service either).
package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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

// CalendarDate is a due date on the wire.
//
// due_date is a DATE column: it names a day, not an instant. Accepting only
// RFC3339 meant the obvious value — "2026-09-01", which is what an HTML date
// input produces and what the column actually stores — failed to unmarshal and
// came back as 400 `invalid_json`, an error that never mentions dates and sends
// the caller looking for a malformed body instead of a missing timestamp.
//
// Both forms are now accepted and both mean the same day at UTC midnight. UTC is
// deliberate: parsing a bare date in a zone behind Greenwich would land on the
// previous day, so an invoice due on the 1st would be stored as due on the 31st.
type CalendarDate struct {
	time.Time
}

const calendarDateLayout = "2006-01-02"

func (d *CalendarDate) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("due_date must be a JSON string, either %q or RFC3339", calendarDateLayout)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		d.Time = time.Time{}
		return nil
	}

	if parsed, err := time.Parse(calendarDateLayout, raw); err == nil {
		d.Time = parsed.UTC()
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fmt.Errorf("due_date %q is not a valid date: expected %q or RFC3339", raw, calendarDateLayout)
	}
	d.Time = parsed.UTC()
	return nil
}

// MarshalJSON keeps the response shape unchanged (RFC3339), so nothing that
// already reads due_date has to change. Only the accepted INPUT widened.
func (d CalendarDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Time)
}

type CreateVendorInvoiceRequest struct {
	TenantID      string       `json:"tenant_id"`
	LegalEntityID string       `json:"legal_entity_id"`
	VendorID      string       `json:"vendor_id"`
	InvoiceNumber string       `json:"invoice_number"`
	Amount        float64      `json:"amount"`
	CurrencyCode  string       `json:"currency_code"`
	DueDate       CalendarDate `json:"due_date"`
	CorrelationID string       `json:"correlation_id"`
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

	// ErrDuplicateInvoiceNumber is returned when (tenant_id, vendor_id,
	// invoice_number) already exists. The table has carried that UNIQUE
	// constraint since 000001, but nothing on the write path recognised the
	// violation: the ON CONFLICT clause covers only the correlation_id index, so
	// a genuine duplicate raised SQLSTATE 23505, fell through to the generic
	// store branch, and was reported as 503 `store_unavailable`. Booking the
	// same vendor invoice twice is a caller mistake with a clear remedy; telling
	// them the database is down sends them to check Docker instead of the
	// number. Distinct from the correlation_id replay, which is a RETRY of one
	// submission and correctly answers 200 with the original.
	ErrDuplicateInvoiceNumber = errorString("an invoice with this number already exists for this vendor")

	// ErrInvalidIdentifier is returned when an id cannot be a UUID at all.
	// Postgres raises SQLSTATE 22P02 from inside the driver for these, which
	// previously surfaced as 503 — a typo in a URL reading as an outage. Each
	// caller maps it to whatever it means for that route: absent for a path
	// param naming a record, rejected for a query filter.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	ErrAuthorizationDenied             = errorString("authorization denied for this invoice action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as general-ledger-svc/schema-registry-svc.
	ErrIdentityMissing = errorString("caller identity missing")
)
