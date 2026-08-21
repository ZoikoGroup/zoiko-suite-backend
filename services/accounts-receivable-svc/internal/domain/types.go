package domain

import "time"

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

// CreateCustomerInvoiceRequest is the input for creating a new invoice.
type CreateCustomerInvoiceRequest struct {
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	CustomerID    string    `json:"customer_id"`
	InvoiceNumber string    `json:"invoice_number"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	DueDate       time.Time `json:"due_date"`
	CorrelationID string    `json:"correlation_id"`
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
