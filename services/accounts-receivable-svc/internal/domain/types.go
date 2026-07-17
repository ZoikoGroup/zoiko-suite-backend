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

	CreatedByPrincipalID        string     `json:"created_by_principal_id"`
	SentByPrincipalID           *string    `json:"sent_by_principal_id,omitempty"`
	MarkedOverdueByPrincipalID  *string    `json:"marked_overdue_by_principal_id,omitempty"`
	PaymentReceivedByPrincipalID *string   `json:"payment_received_by_principal_id,omitempty"`
	CorrelationID               string     `json:"correlation_id"`
	CreatedAt                   time.Time  `json:"created_at"`
	SentAt                      *time.Time `json:"sent_at,omitempty"`
	MarkedOverdueAt             *time.Time `json:"marked_overdue_at,omitempty"`
	PaymentReceivedAt           *time.Time `json:"payment_received_at,omitempty"`
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
type ListInvoicesFilter struct {
	TenantID      string
	LegalEntityID string
	CustomerID    string
	Status        string
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrInvoiceNotFound         = errorString("customer invoice not found")
	ErrInvalidTransition       = errorString("invalid invoice status transition")
	ErrStoreUnavailable        = errorString("accounts receivable store unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for this invoice action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrLedgerVerificationFailed = errorString("ledger verification failed: no matching finalized journal found")
)
