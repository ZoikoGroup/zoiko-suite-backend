package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// InvoiceStatus is the receivable-side lifecycle: ISSUED -> SENT -> {OVERDUE | PAID}.
// PAID is terminal.
type InvoiceStatus string

const (
	InvoiceStatusIssued  InvoiceStatus = "ISSUED"
	InvoiceStatusSent    InvoiceStatus = "SENT"
	InvoiceStatusOverdue InvoiceStatus = "OVERDUE"
	InvoiceStatusPaid    InvoiceStatus = "PAID"
)

// CustomerInvoice models a customer invoice header.
type CustomerInvoice struct {
	InvoiceID     string        `json:"invoice_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	CustomerID    string        `json:"customer_id"`
	InvoiceNumber string        `json:"invoice_number"`
	Amount        float64       `json:"amount"`
	CurrencyCode  string        `json:"currency_code"`
	DueDate       time.Time     `json:"due_date"`
	Status        InvoiceStatus `json:"status"`

	// ── AR-05 required business/source inputs (§9.F) ──────────────────────

	// InvoiceDate is the date on the document we issued. Distinct from
	// CreatedAt (when we raised it) and DueDate (when it must be paid): all
	// three legitimately differ and each answers a different question.
	InvoiceDate CalendarDate `json:"invoice_date"`

	// SupplyDate is the tax point — when the supply took place. Drives which
	// tax period and rule version apply, and is routinely in a different month
	// from the invoice date on a supply invoiced in arrears.
	SupplyDate CalendarDate `json:"supply_date"`

	// NetAmount and TaxAmount split the existing Amount, which keeps its
	// meaning as the gross total receivable. The service enforces
	// net + tax == gross, and that the lines sum to each — the AR equivalent of
	// a balance check.
	NetAmount float64 `json:"net_amount"`
	TaxAmount float64 `json:"tax_amount"`

	// InvoiceDocumentID is the customer-facing document in document-vault-svc.
	// Required to leave ISSUED, not to enter it — an invoice keyed ahead of its
	// scan is an ordinary working state, one SENT without it is the audit gap.
	InvoiceDocumentID *string `json:"invoice_document_id,omitempty"`

	// SalesOrderID and CustomerBillingRef are the AR side of AP-05's PO
	// references. Both are carried unvalidated: no sales-order service exists,
	// so nothing can confirm either. SalesOrderID answers AR-06's matching;
	// CustomerBillingRef is the customer's own reference for the statement.
	SalesOrderID       *string `json:"sales_order_id,omitempty"`
	CustomerBillingRef *string `json:"customer_billing_ref,omitempty"`

	// AR-08 cash application. PaymentDate and PaymentReference are what the
	// customer supplied when the money arrived; optional, because a payment
	// with neither is still a payment, and the lifecycle stamp is what an audit
	// reaches for first.
	PaymentDate      *CalendarDate `json:"payment_date,omitempty"`
	PaymentReference *string       `json:"payment_reference,omitempty"`

	// Lines is populated by the read paths. Nil on a pre-contract invoice
	// recorded before migration 000006, which is a real historical state rather
	// than a fault — see IsPreContract.
	Lines []CustomerInvoiceLine `json:"lines,omitempty"`

	// ──────────────────────────────────────────────────────────────────────

	CreatedByPrincipalID         string     `json:"created_by_principal_id"`
	SentByPrincipalID            *string    `json:"sent_by_principal_id,omitempty"`
	MarkedOverdueByPrincipalID   *string    `json:"marked_overdue_by_principal_id,omitempty"`
	PaymentReceivedByPrincipalID *string    `json:"payment_received_by_principal_id,omitempty"`
	CorrelationID                string     `json:"correlation_id"`
	CreatedAt                    time.Time  `json:"created_at"`
	SentAt                       *time.Time `json:"sent_at,omitempty"`
	MarkedOverdueAt              *time.Time `json:"marked_overdue_at,omitempty"`
	PaymentReceivedAt            *time.Time `json:"payment_received_at,omitempty"`
}

// CustomerInvoiceLine is one line of a customer invoice — AR-05's "lines" and
// "tax" inputs.
//
// Per-line tax because one invoice routinely carries two treatments: a
// standard-rated item and a zero-rated one on the same document. A header-only
// tax figure cannot express that, and cannot be handed to TAX-03 or to account
// mapping.
type CustomerInvoiceLine struct {
	InvoiceLineID string `json:"invoice_line_id"`
	InvoiceID     string `json:"invoice_id"`
	LineNumber    int    `json:"line_number"`

	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	NetAmount   float64 `json:"net_amount"`

	TaxCode   *string `json:"tax_code,omitempty"`
	TaxAmount float64 `json:"tax_amount"`

	// SalesOrderLineRef is which sales order line this answers, for AR-06 when
	// it exists. Unvalidated: no sales-order service exposes line detail.
	SalesOrderLineRef *string `json:"sales_order_line_ref,omitempty"`

	// Dimensions is free-form for the same reason as general-ledger-svc's
	// journal line dimensions: REF-08 Financial Dimension Registry does not
	// exist, so nothing says which dimensions a tenant has defined.
	Dimensions Dimensions `json:"dimensions,omitempty"`
}

// IsPreContract reports whether this invoice predates migration 000006, which
// added AR-05's line and tax inputs.
//
// Such an invoice has a gross amount and no account of what it was for. That is
// a real historical state, not a fault, and the console names it rather than
// rendering an empty line table as though the data were missing.
func (c CustomerInvoice) IsPreContract() bool { return len(c.Lines) == 0 }

// ── wire types (request bodies) ─────────────────────────────────────────────

// CalendarDate is a date on the wire.
//
// due_date, invoice_date, supply_date and payment_date are DATE columns: they
// name a day, not an instant. Accepting only RFC3339 meant the obvious value —
// "2026-09-01", which is what an HTML date input produces and what the column
// actually stores — failed to unmarshal and came back as 400 `invalid_json`, an
// error that never mentions dates and sends the caller looking for a malformed
// body instead of a missing timestamp.
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
		return fmt.Errorf("date must be a JSON string, either %q or RFC3339", calendarDateLayout)
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
		return fmt.Errorf("date %q is not a valid date: expected %q or RFC3339", raw, calendarDateLayout)
	}
	d.Time = parsed.UTC()
	return nil
}

// MarshalJSON keeps the response shape unchanged (RFC3339), so nothing that
// already reads a date has to change. Only the accepted INPUT widened.
func (d CalendarDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Time)
}

// Value implements driver.Valuer for the DATE columns.
func (d CalendarDate) Value() (driver.Value, error) {
	if d.Time.IsZero() {
		return nil, nil
	}
	return d.Time, nil
}

// Scan implements sql.Scanner for the DATE columns.
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

// CreateCustomerInvoiceLineInput is one line on the way in.
type CreateCustomerInvoiceLineInput struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity,omitempty"`
	UnitPrice   float64 `json:"unit_price,omitempty"`
	NetAmount   float64 `json:"net_amount"`

	TaxCode           *string    `json:"tax_code,omitempty"`
	TaxAmount         float64    `json:"tax_amount,omitempty"`
	SalesOrderLineRef *string    `json:"sales_order_line_ref,omitempty"`
	Dimensions        Dimensions `json:"dimensions,omitempty"`
}

// CreateCustomerInvoiceRequest is the input for creating a new invoice.
type CreateCustomerInvoiceRequest struct {
	TenantID      string       `json:"tenant_id"`
	LegalEntityID string       `json:"legal_entity_id"`
	CustomerID    string       `json:"customer_id"`
	InvoiceNumber string       `json:"invoice_number"`
	Amount        float64      `json:"amount"`
	CurrencyCode  string       `json:"currency_code"`
	DueDate       CalendarDate `json:"due_date"`
	CorrelationID string       `json:"correlation_id"`

	// ── AR-05 required business/source inputs ─────────────────────────────

	InvoiceDate CalendarDate `json:"invoice_date"`
	SupplyDate  CalendarDate `json:"supply_date"`

	// At least one line. Amount stays the gross total and must equal the lines'
	// net plus their tax — see ErrInvoiceDoesNotBalance.
	Lines []CreateCustomerInvoiceLineInput `json:"lines"`

	// Optional. InvoiceDocumentID is the customer-facing document in
	// document-vault-svc; SalesOrderID and CustomerBillingRef are carried
	// unvalidated.
	InvoiceDocumentID   *string `json:"invoice_document_id,omitempty"`
	SalesOrderID        *string `json:"sales_order_id,omitempty"`
	CustomerBillingRef  *string `json:"customer_billing_ref,omitempty"`
}

// RecordCustomerPaymentRequest is the AR-08 cash-application payload on
// POST /v1/invoices/{invoice_id}/pay.
type RecordCustomerPaymentRequest struct {
	// PaymentDate is when the money actually arrived, when the caller knows it.
	// Optional — the lifecycle anyway stamps when payment was RECORDED.
	PaymentDate CalendarDate `json:"payment_date,omitempty"`

	// PaymentReference is the customer's reference for the payment (remittance
	// advice, bank reference, …). Optional, carried for the customer's own
	// reconciliation.
	PaymentReference string `json:"payment_reference,omitempty"`
}

// LineTotals sums the request's lines.
func (r CreateCustomerInvoiceRequest) LineTotals() (net, tax float64) {
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
// than floats for its double-entry check, and the same reason
// accounts-payable-svc compares the same way.
func (r CreateCustomerInvoiceRequest) Balances() bool {
	net, tax := r.LineTotals()
	return cents(net)+cents(tax) == cents(r.Amount)
}

func cents(v float64) int64 {
	if v < 0 {
		return -int64(-v*100 + 0.5)
	}
	return int64(v*100 + 0.5)
}

// ListInvoicesFilter contains filters for querying invoices.
//
// Limit and Offset are not optional in practice: the register read used to be
// unbounded, returning every invoice a tenant had ever raised on every dashboard
// load. The handler always sets Limit (see defaultLimit), so a zero here means a
// caller inside this service forgot to.
type ListInvoicesFilter struct {
	TenantID      string
	LegalEntityID string
	CustomerID    string
	Status        string
	Limit         int
	Offset        int
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrInvoiceNotFound          = errorString("customer invoice not found")
	ErrInvalidTransition        = errorString("invalid invoice status transition")
	ErrStoreUnavailable         = errorString("accounts receivable store unavailable")
	ErrAuthorizationDenied      = errorString("authorization denied for this invoice action")
	ErrAuthzServiceUnavailable  = errorString("authorization-svc unavailable")
	ErrIdentityMissing          = errorString("caller identity missing")
	ErrLedgerVerificationFailed = errorString("ledger verification failed: no matching finalized journal found")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope. Every route needs one: the register is tenant-isolated,
	// and a read with no scope has no honest answer — it must not quietly
	// become "all tenants" (which is what ListInvoices did) or "not found"
	// (which is what GetInvoice did).
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the caller's verified scope — as a tenant_id query parameter on the
	// register, or as tenant_id in a create body. Both used to be BELIEVED in
	// preference to the verified header, which made the whole register
	// readable, and writable, across tenants.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")

	// ErrLegalEntityNotInTenant is returned when the legal entity named on a write
	// does not belong to the caller's verified tenant — or does not exist at all.
	// The two are deliberately one answer: telling them apart would make this
	// endpoint an oracle for which entity ids exist in other tenants. The
	// distinction is kept in the service log instead.
	ErrLegalEntityNotInTenant = errorString("legal entity is not in the caller's tenant")

	// ErrNotYetDue is returned when an invoice is marked overdue before its
	// due date has passed. receivable.overdue feeds aging and impairment
	// downstream, so an invoice that is merely unpaid must not be able to
	// present itself as late.
	ErrNotYetDue = errorString("invoice cannot be marked overdue before its due date has passed")

	// ErrDuplicateInvoiceNumber is returned when (tenant_id, customer_id,
	// invoice_number) collides. 000001 has declared that UNIQUE constraint since
	// the service was written, but nothing recognised its violation: SQLSTATE
	// 23505 fell through to the generic store error and the handler answered 503
	// store_unavailable, so re-keying an invoice number was indistinguishable
	// from the database being down — an answer that is wrong about whose problem
	// it is and offers no remedy. Same defect, and same fix, as
	// accounts-payable-svc's.
	ErrDuplicateInvoiceNumber = errorString("an invoice with this number already exists for this customer")

	// ErrInvalidIdentifier is returned when a value compared against a uuid
	// column is not a UUID. tenant_id and legal_entity_id are both UUID columns,
	// so a non-UUID dies inside the driver (SQLSTATE 22P02) before any row is
	// examined — which also answered 503. The console's legacy client sent
	// "tenant-zoiko-dev-01" and "le-singapore-01", so every create it ever made
	// landed here and was reported as a dead store.
	ErrInvalidIdentifier = errorString("tenant_id and legal_entity_id must both be UUIDs")

	// ErrInvalidStatusFilter is returned for a ?status= that is not one of the
	// four lifecycle states. Passing it through matched no row and answered an
	// empty register, so a typo read as "this tenant has no invoices".
	ErrInvalidStatusFilter = errorString("status must be one of ISSUED, SENT, OVERDUE, PAID")

	// ── AR-05 input contract ──────────────────────────────────────────────

	ErrNoLines = errorString("a customer invoice must have at least one line")

	// ErrInvoiceDoesNotBalance is the AR equivalent of the ledger's
	// double-entry check: what the lines say the invoice is for has to add up
	// to what it says is receivable. Without it a line detail can drift from the
	// total and nothing downstream — matching, tax, account mapping — would
	// notice.
	ErrInvoiceDoesNotBalance = errorString("lines do not account for the invoice amount: sum(net) + sum(tax) must equal amount")

	ErrInvalidLine = errorString("each line needs a description and a net_amount, and neither net_amount nor tax_amount may be negative")

	// ErrInvoiceDateRequired and ErrSupplyDateRequired name the absence of a
	// check as a decision rather than an oversight: a supply date BEFORE the
	// invoice date is entirely ordinary (invoiced in arrears), and one after it
	// is also legitimate (invoiced in advance). Only an absent one is refused.
	ErrInvoiceDateRequired = errorString("invoice_date is required — the date on the document we issued")
	ErrSupplyDateRequired  = errorString("supply_date is required — the tax point decides which tax period and rule version apply")

	// ErrInvoiceDocumentRequired enforces the evidence gate at the SEND
	// boundary rather than at intake. An invoice keyed ahead of its scan is a
	// legitimate working state; one SENT without the customer's document is
	// the audit gap, exactly as INV-10 frames it on the supplier side.
	ErrInvoiceDocumentRequired = errorString("invoice_document_id is required to send: an invoice cannot be asserted issued without the document")

	// ErrSelfPaymentNotAllowed enforces the platform's Segregation of Duties
	// doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3), applied to the
	// cash-handling half of the lifecycle: the principal who issued an invoice
	// may not be the principal who records its payment. Billing is not
	// treasury, and the pair must be separable in the audit trail.
	ErrSelfPaymentNotAllowed = errorString("principal may not record payment against an invoice they issued")
)

// ValidInvoiceStatus reports whether s is one of the four lifecycle states.
func ValidInvoiceStatus(s string) bool {
	switch InvoiceStatus(s) {
	case InvoiceStatusIssued, InvoiceStatusSent, InvoiceStatusOverdue, InvoiceStatusPaid:
		return true
	default:
		return false
	}
}