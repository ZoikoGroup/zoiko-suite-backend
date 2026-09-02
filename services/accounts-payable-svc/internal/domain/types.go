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
	"database/sql/driver"
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
	InvoiceStatusReceived         InvoiceStatus = "RECEIVED"
	InvoiceStatusValidated        InvoiceStatus = "VALIDATED"
	InvoiceStatusApproved         InvoiceStatus = "APPROVED"
	InvoiceStatusPaymentRequested InvoiceStatus = "PAYMENT_REQUESTED"
)

// ValidInvoiceStatus reports whether s is a status this service can ever have
// stored. An unrecognised ?status= filter matched no row and returned an empty
// list, so a typo was indistinguishable from a tenant with no payables.
func ValidInvoiceStatus(s string) bool {
	switch InvoiceStatus(s) {
	case InvoiceStatusReceived, InvoiceStatusValidated, InvoiceStatusApproved, InvoiceStatusPaymentRequested:
		return true
	default:
		return false
	}
}

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

	// ── AP-05 required business/source inputs (§9.F) ──────────────────────

	// InvoiceDate is the date on the supplier's document. Distinct from
	// CreatedAt (when we received it) and DueDate (when it must be paid): all
	// three legitimately differ and each answers a different question.
	InvoiceDate CalendarDate `json:"invoice_date"`

	// SupplyDate is the tax point — when the supply took place. Drives which
	// tax period and rule version apply, and is routinely in a different month
	// from the invoice date on a supply invoiced in arrears.
	SupplyDate CalendarDate `json:"supply_date"`

	// NetAmount and TaxAmount split the existing Amount, which keeps its
	// meaning as the gross total payable. The service enforces
	// net + tax == gross, and that the lines sum to each — the AP equivalent of
	// a balance check.
	NetAmount float64 `json:"net_amount"`
	TaxAmount float64 `json:"tax_amount"`

	// PurchaseOrderID is validated against purchase-order-svc when supplied: it
	// must exist, belong to the same legal entity and not be closed.
	//
	// GoodsReceiptRef is carried unvalidated — AP-04 Goods/Service Receipt does
	// not exist, so nothing can confirm a receipt happened. Without it there is
	// no third leg for a three-way match.
	PurchaseOrderID *string `json:"purchase_order_id,omitempty"`
	GoodsReceiptRef *string `json:"goods_receipt_ref,omitempty"`

	// POVendorProfileID is copied from the purchase order at intake so a
	// disagreement between the PO's supplier and this invoice's is visible.
	// It does not refuse the invoice — see the migration for why.
	POVendorProfileID *string `json:"po_vendor_profile_id,omitempty"`

	// InvoiceDocumentID is the supplier's actual document in document-vault-svc.
	// Required to leave RECEIVED, not to enter it: §7 makes a draft an editable
	// working state and INV-10 requires evidence before COMPLETION, so an
	// invoice keyed ahead of its scan is legitimate while one VALIDATED without
	// a document is the audit gap.
	InvoiceDocumentID *string `json:"invoice_document_id,omitempty"`

	// Lines is populated by the read paths. Nil on a pre-contract invoice
	// recorded before migration 000006, which is a real historical state rather
	// than a fault — see IsPreContract.
	Lines []VendorInvoiceLine `json:"lines,omitempty"`

	// ──────────────────────────────────────────────────────────────────────

	// SourceContractID is nil unless this invoice was issued against a real
	// contract-lifecycle-svc contract.
	SourceContractID *string `json:"source_contract_id,omitempty"`

	CreatedByPrincipalID          string     `json:"created_by_principal_id"`
	ValidatedByPrincipalID        *string    `json:"validated_by_principal_id,omitempty"`
	ApprovedByPrincipalID         *string    `json:"approved_by_principal_id,omitempty"`
	PaymentRequestedByPrincipalID *string    `json:"payment_requested_by_principal_id,omitempty"`
	CorrelationID                 string     `json:"correlation_id"`
	CreatedAt                     time.Time  `json:"created_at"`
	ValidatedAt                   *time.Time `json:"validated_at,omitempty"`
	ApprovedAt                    *time.Time `json:"approved_at,omitempty"`
	PaymentRequestedAt            *time.Time `json:"payment_requested_at,omitempty"`
}

// VendorInvoiceLine is one line of a supplier invoice — AP-05's "lines" and
// "tax" inputs.
//
// Per-line tax because one invoice routinely carries two treatments: a
// standard-rated item and a zero-rated one on the same document. A header-only
// tax figure cannot express that, and cannot be handed to TAX-03 or to account
// mapping.
type VendorInvoiceLine struct {
	InvoiceLineID string `json:"invoice_line_id"`
	InvoiceID     string `json:"invoice_id"`
	LineNumber    int    `json:"line_number"`

	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	NetAmount   float64 `json:"net_amount"`

	TaxCode   *string `json:"tax_code,omitempty"`
	TaxAmount float64 `json:"tax_amount"`

	// TaxDeterminationID links to the tax-determination-svc determination this
	// line's tax came from, when one was run. Nil for tax keyed straight from
	// the supplier's document, which is the common case today — which is why
	// this is a link rather than a requirement.
	TaxDeterminationID *string `json:"tax_determination_id,omitempty"`

	// POLineReference is which purchase-order line this answers, for AP-06
	// Invoice Matching when it exists. Unvalidated: purchase-order-svc exposes
	// no line detail.
	POLineReference *string `json:"po_line_reference,omitempty"`

	// Dimensions is free-form for the same reason as general-ledger-svc's
	// journal line dimensions: REF-08 Financial Dimension Registry does not
	// exist, so nothing says which dimensions a tenant has defined.
	Dimensions Dimensions `json:"dimensions,omitempty"`
}

// IsPreContract reports whether this invoice predates migration 000006, which
// added AP-05's line and tax inputs.
//
// Such an invoice has a gross amount and no account of what it was for. That is
// a real historical state, not a fault, and the console names it rather than
// rendering an empty line table as though the data were missing.
func (v VendorInvoice) IsPreContract() bool { return len(v.Lines) == 0 }

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

// Value implements driver.Valuer for the DATE columns.
//
// Without it pgx has no encoder for this struct: it embeds time.Time but is not
// one, so a CalendarDate parameter went to Postgres as NULL and a scan of a DATE
// back into it failed outright with "cannot scan date (OID 1082) in binary
// format into *domain.CalendarDate". Every read of invoice_date errored.
//
// The zero value is NULL rather than year zero, so "no date supplied" stays
// distinguishable from a real date. Same shape as general-ledger-svc's
// domain.Date, which is the pattern this follows.
func (d CalendarDate) Value() (driver.Value, error) {
	if d.Time.IsZero() {
		return nil, nil
	}
	return d.Time, nil
}

// Scan implements sql.Scanner for the DATE columns.
//
// pgx hands a DATE back as time.Time; the string cases cover a driver configured
// to return dates as text, which is a supported pgx mode. The day is rebuilt at
// UTC midnight for the reason the type comment gives - a bare date read in a
// zone behind Greenwich lands on the previous day.
func (d *CalendarDate) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		d.Time = time.Time{}
		return nil
	case time.Time:
		d.Time = time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
		return nil
	case string:
		parsed, err := time.Parse(calendarDateLayout, strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("cannot scan %q into CalendarDate: expected %q", v, calendarDateLayout)
		}
		d.Time = parsed.UTC()
		return nil
	case []byte:
		return d.Scan(string(v))
	default:
		return fmt.Errorf("cannot scan %T into CalendarDate", src)
	}
}

// CreateVendorInvoiceLineInput is one line on the way in.
type CreateVendorInvoiceLineInput struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity,omitempty"`
	UnitPrice   float64 `json:"unit_price,omitempty"`
	NetAmount   float64 `json:"net_amount"`

	TaxCode            *string    `json:"tax_code,omitempty"`
	TaxAmount          float64    `json:"tax_amount,omitempty"`
	TaxDeterminationID *string    `json:"tax_determination_id,omitempty"`
	POLineReference    *string    `json:"po_line_reference,omitempty"`
	Dimensions         Dimensions `json:"dimensions,omitempty"`
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

	// ── AP-05 required business/source inputs ─────────────────────────────

	InvoiceDate CalendarDate `json:"invoice_date"`
	SupplyDate  CalendarDate `json:"supply_date"`

	// At least one line. Amount stays the gross total and must equal the lines'
	// net plus their tax — see ErrInvoiceDoesNotBalance.
	Lines []CreateVendorInvoiceLineInput `json:"lines"`

	// Optional. PurchaseOrderID is validated against purchase-order-svc when
	// present; the other two are carried.
	PurchaseOrderID   *string `json:"purchase_order_id,omitempty"`
	GoodsReceiptRef   *string `json:"goods_receipt_ref,omitempty"`
	InvoiceDocumentID *string `json:"invoice_document_id,omitempty"`

	// SourceContractID is optional: the contract-lifecycle-svc contract
	// this invoice was issued against, when one exists.
	SourceContractID *string `json:"source_contract_id,omitempty"`
}

// LineTotals sums the request's lines.
func (r CreateVendorInvoiceRequest) LineTotals() (net, tax float64) {
	for _, l := range r.Lines {
		net += l.NetAmount
		tax += l.TaxAmount
	}
	return net, tax
}

// Balances reports whether the lines account for the gross amount.
//
// Compared in minor units. Summing decimal amounts in binary floating point
// leaves 0.1 + 0.2 != 0.3, and this comparison decides whether an invoice is
// accepted — the same reason general-ledger-svc compares exact cents rather
// than floats for its double-entry check.
//
// (The float64 fields are themselves an INV-04 problem this service shares with
// the ledger; see docs/architecture/input-contract-conformance.md gap G-2.
// Rounding to cents here is the honest comparison available until the money
// type changes.)
func (r CreateVendorInvoiceRequest) Balances() bool {
	net, tax := r.LineTotals()
	return cents(net)+cents(tax) == cents(r.Amount)
}

func cents(v float64) int64 {
	if v < 0 {
		return -int64(-v*100 + 0.5)
	}
	return int64(v*100 + 0.5)
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

	// ── AP-05 input contract ─────────────────────────────────────────────

	ErrNoLines = errorString("a supplier invoice must have at least one line")

	// ErrInvoiceDoesNotBalance is the AP equivalent of the ledger's
	// double-entry check: what the lines say the invoice is for has to add up
	// to what it says is payable. Without it a line detail can drift from the
	// total and nothing downstream — matching, tax, account mapping — would
	// notice.
	ErrInvoiceDoesNotBalance = errorString("lines do not account for the invoice amount: sum(net) + sum(tax) must equal amount")

	ErrInvalidLine = errorString("each line needs a description and a net_amount, and neither net_amount nor tax_amount may be negative")

	// ErrSupplyBeforeInvoiceImpossible has no equivalent constraint: a supply
	// date BEFORE the invoice date is entirely ordinary (invoiced in arrears),
	// and a supply date after it is also legitimate (invoiced in advance). Only
	// an absent one is refused. Named here so the absence of a check is a
	// decision on the record rather than an oversight.
	ErrInvoiceDateRequired = errorString("invoice_date is required — the date on the supplier's document")
	ErrSupplyDateRequired  = errorString("supply_date is required — the tax point decides which tax period and rule version apply")

	// ErrInvoiceDocumentRequired enforces INV-10 at the completion boundary
	// rather than at intake. An invoice keyed ahead of its scan is a legitimate
	// draft; one VALIDATED with no document is the audit gap.
	ErrInvoiceDocumentRequired = errorString("invoice_document_id is required to validate: an invoice cannot be asserted complete without the supplier's document")

	// ErrPurchaseOrderUnknown means purchase-order-svc does not recognise the
	// referenced PO, or it belongs to another legal entity.
	ErrPurchaseOrderUnknown = errorString("purchase_order_id is not recognised by purchase-order-svc for this legal entity")

	// ErrPurchaseOrderClosed means the PO exists but is no longer open to
	// invoicing.
	ErrPurchaseOrderClosed = errorString("purchase order is closed and cannot accept further invoices")

	// ErrPurchaseOrderUnverifiable means purchase-order-svc could not be
	// reached. Distinct from Unknown on purpose: "cannot answer" is never "no",
	// and an invoice referencing an unverified PO fails closed rather than
	// being recorded against a commitment nobody confirmed.
	ErrPurchaseOrderUnverifiable = errorString("purchase order could not be verified — purchase-order-svc unreachable")

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

	// ErrSelfApprovalNotAllowed enforces the platform's Segregation of Duties
	// doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3): the principal
	// who created a record may not be the same principal who approves,
	// executes, or passes it.
	ErrSelfApprovalNotAllowed = errorString("principal may not approve or decide on their own submission")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope (no X-Tenant-Id). Every invoice is tenant-owned, and a read
	// with no scope has no honest answer — it must not quietly become "whatever
	// tenant the caller named", which is what ListInvoices did.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the caller's verified scope — as ?tenant_id= when listing the
	// register, or as tenant_id in a create body. Both used to be BELIEVED in
	// preference to the header (which was never read at all), which made the
	// whole payables register readable, and writable, across tenants.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")
)
