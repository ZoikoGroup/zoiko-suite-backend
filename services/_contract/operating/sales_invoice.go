package operating

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// InvoiceStatus represents the lifecycle of a sales invoice.
type InvoiceStatus string

const (
	InvoiceStatusDraft      InvoiceStatus = "DRAFT"
	InvoiceStatusApproved   InvoiceStatus = "APPROVED"
	InvoiceStatusIssued     InvoiceStatus = "ISSUED"
	InvoiceStatusPartPaid   InvoiceStatus = "PART_PAID"
	InvoiceStatusPaid       InvoiceStatus = "PAID"
	InvoiceStatusDisputed   InvoiceStatus = "DISPUTED"
	InvoiceStatusCredited   InvoiceStatus = "CREDITED"
	InvoiceStatusVoided     InvoiceStatus = "VOIDED"
)

// OpenItemStatus represents the settlement state of an open receivable or payable item.
type OpenItemStatus string

const (
	OpenItemStatusOpen        OpenItemStatus = "OPEN"
	OpenItemStatusPartSettled OpenItemStatus = "PART_SETTLED"
	OpenItemStatusSettled     OpenItemStatus = "SETTLED"
	OpenItemStatusDisputed    OpenItemStatus = "DISPUTED"
	OpenItemStatusWrittenOff  OpenItemStatus = "WRITTEN_OFF"
)

// CashReceiptStatus represents the application state of a customer payment receipt.
type CashReceiptStatus string

const (
	CashReceiptStatusUnapplied   CashReceiptStatus = "UNAPPLIED"
	CashReceiptStatusPartApplied CashReceiptStatus = "PART_APPLIED"
	CashReceiptStatusApplied     CashReceiptStatus = "APPLIED"
	CashReceiptStatusReturned    CashReceiptStatus = "RETURNED"
)

// SalesInvoice represents an issued receivable commercial document from a legal entity to a customer (AR-INV).
type SalesInvoice struct {
	SalesInvoiceID       types.UUID         `json:"sales_invoice_id"`
	TenantID             types.UUID         `json:"tenant_id"`
	LegalEntityID        types.UUID         `json:"legal_entity_id"`
	CustomerPartyRoleID  types.UUID         `json:"customer_party_role_id"` // Points to canonical PartyRole (RoleTypeCustomer)
	InvoiceNumber        string             `json:"invoice_number"`
	DocumentDate         time.Time          `json:"document_date"`
	TaxPointDate         *time.Time         `json:"tax_point_date,omitempty"`
	DueDate              time.Time          `json:"due_date"`
	CurrencyCode         string             `json:"currency_code"` // ISO 4217
	NetAmount            types.MoneyDecimal `json:"net_amount"`
	TaxAmount            types.MoneyDecimal `json:"tax_amount"`
	GrossAmount          types.MoneyDecimal `json:"gross_amount"`
	Status               InvoiceStatus      `json:"status"`
	TaxDeterminationID   *types.UUID        `json:"tax_determination_id,omitempty"`
	Lines                []SalesInvoiceLine `json:"lines,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
}

// SalesInvoiceLine represents a line item on a sales invoice (AR-INVL).
type SalesInvoiceLine struct {
	SalesInvoiceLineID    types.UUID         `json:"sales_invoice_line_id"`
	SalesInvoiceID        types.UUID         `json:"sales_invoice_id"`
	LineNo                int                `json:"line_no"`
	Description           string             `json:"description"`
	Quantity              types.MoneyDecimal `json:"quantity"` // Exact quantity
	UnitPrice             types.MoneyDecimal `json:"unit_price"`
	DiscountAmount        types.MoneyDecimal `json:"discount_amount"`
	NetAmount             types.MoneyDecimal `json:"net_amount"`
	TaxClassificationCode string             `json:"tax_classification_code,omitempty"`
}

// ReceivableOpenItem tracks the outstanding receivable state derived from issued invoices (AR-OPEN).
// It adjusts downward as cash applications or credit notes are applied.
type ReceivableOpenItem struct {
	ReceivableOpenItemID types.UUID         `json:"receivable_open_item_id"`
	TenantID             types.UUID         `json:"tenant_id"`
	LegalEntityID        types.UUID         `json:"legal_entity_id"`
	SourceDocumentID     types.UUID         `json:"source_document_id"` // sales_invoice_id
	CustomerPartyRoleID  types.UUID         `json:"customer_party_role_id"`
	OriginalAmount       types.MoneyDecimal `json:"original_amount"`
	OpenAmount           types.MoneyDecimal `json:"open_amount"` // Residual amount
	CurrencyCode         string             `json:"currency_code"`
	DueDate              time.Time          `json:"due_date"`
	Status               OpenItemStatus     `json:"status"`
}

// ApplyPayment decreases the open amount by appliedAmount.
// Enforces Anti-Pattern prevention: cannot over-settle beyond current open amount.
func (roi *ReceivableOpenItem) ApplyPayment(appliedAmount types.MoneyDecimal) error {
	if appliedAmount.Sign() <= 0 {
		return errors.New("applied amount must be strictly positive")
	}
	if appliedAmount.Cmp(roi.OpenAmount) > 0 {
		return fmt.Errorf("over-settlement prohibited: applied amount (%s) exceeds open amount (%s)",
			appliedAmount.StringTrimmed(), roi.OpenAmount.StringTrimmed())
	}

	roi.OpenAmount = roi.OpenAmount.Sub(appliedAmount)
	if roi.OpenAmount.IsZero() {
		roi.Status = OpenItemStatusSettled
	} else {
		roi.Status = OpenItemStatusPartSettled
	}
	return nil
}

// CashReceipt represents a customer payment received into a bank account (AR-RECEIPT).
type CashReceipt struct {
	CashReceiptID     types.UUID         `json:"cash_receipt_id"`
	TenantID          types.UUID         `json:"tenant_id"`
	LegalEntityID     types.UUID         `json:"legal_entity_id"`
	BankTransactionID *types.UUID        `json:"bank_transaction_id,omitempty"`
	PayerPartyID      *types.UUID        `json:"payer_party_id,omitempty"`
	Amount            types.MoneyDecimal `json:"amount"`
	UnappliedAmount   types.MoneyDecimal `json:"unapplied_amount"`
	CurrencyCode      string             `json:"currency_code"`
	ReceivedAt        time.Time          `json:"received_at"`
	Reference         string             `json:"reference,omitempty"`
	Status            CashReceiptStatus  `json:"status"`
}

// CashApplication represents the allocation of a cash receipt to a receivable open item (AR-APPLY).
type CashApplication struct {
	CashApplicationID    types.UUID         `json:"cash_application_id"`
	TenantID             types.UUID         `json:"tenant_id"`
	CashReceiptID        types.UUID         `json:"cash_receipt_id"`
	ReceivableOpenItemID types.UUID         `json:"receivable_open_item_id"`
	AppliedAmount        types.MoneyDecimal `json:"applied_amount"`
	ApplicationDate      time.Time          `json:"application_date"`
	FxDifferenceAmount   types.MoneyDecimal `json:"fx_difference_amount,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
}
