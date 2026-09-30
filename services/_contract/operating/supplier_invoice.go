package operating

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// MatchingStatus represents the PO matching status for 3-way matching.
type MatchingStatus string

const (
	MatchingStatusUnmatched MatchingStatus = "UNMATCHED"
	MatchingStatusMatched   MatchingStatus = "MATCHED"
	MatchingStatusException MatchingStatus = "EXCEPTION"
	MatchingStatusApproved  MatchingStatus = "APPROVED"
)

// SupplierInvoiceStatus represents the AP invoice lifecycle.
type SupplierInvoiceStatus string

const (
	SupplierInvoiceStatusCaptured       SupplierInvoiceStatus = "CAPTURED"
	SupplierInvoiceStatusValidated      SupplierInvoiceStatus = "VALIDATED"
	SupplierInvoiceStatusApproved       SupplierInvoiceStatus = "APPROVED"
	SupplierInvoiceStatusPaymentRequested SupplierInvoiceStatus = "PAYMENT_REQUESTED"
	SupplierInvoiceStatusPaid           SupplierInvoiceStatus = "PAID"
	SupplierInvoiceStatusRejected       SupplierInvoiceStatus = "REJECTED"
)

// SupplierInvoice represents a supplier payable document (AP-INV).
type SupplierInvoice struct {
	SupplierInvoiceID     types.UUID            `json:"supplier_invoice_id"`
	TenantID              types.UUID            `json:"tenant_id"`
	LegalEntityID         types.UUID            `json:"legal_entity_id"`
	SupplierPartyRoleID   types.UUID            `json:"supplier_party_role_id"` // Points to canonical PartyRole (RoleTypeSupplier)
	SupplierInvoiceNumber string                `json:"supplier_invoice_number"`
	DocumentDate          time.Time             `json:"document_date"`
	DueDate               time.Time             `json:"due_date"`
	CurrencyCode          string                `json:"currency_code"` // ISO 4217
	GrossAmount           types.MoneyDecimal    `json:"gross_amount"`
	PurchaseOrderID       *types.UUID           `json:"purchase_order_id,omitempty"`
	MatchingStatus        MatchingStatus        `json:"matching_status"`
	Status                SupplierInvoiceStatus `json:"status"`
	Lines                 []SupplierInvoiceLine `json:"lines,omitempty"`
	CreatedAt             time.Time             `json:"created_at"`
}

// SupplierInvoiceLine represents a line item on a supplier invoice (AP-INVL).
type SupplierInvoiceLine struct {
	SupplierInvoiceLineID types.UUID         `json:"supplier_invoice_line_id"`
	SupplierInvoiceID     types.UUID         `json:"supplier_invoice_id"`
	LineNo                int                `json:"line_no"`
	Description           string             `json:"description"`
	Quantity              types.MoneyDecimal `json:"quantity"`
	UnitPrice             types.MoneyDecimal `json:"unit_price"`
	NetAmount             types.MoneyDecimal `json:"net_amount"`
	PurchaseOrderLineID   *types.UUID        `json:"purchase_order_line_id,omitempty"`
}

// PayableOpenItem represents an outstanding supplier liability item (AP-OPEN).
type PayableOpenItem struct {
	PayableOpenItemID   types.UUID         `json:"payable_open_item_id"`
	TenantID            types.UUID         `json:"tenant_id"`
	LegalEntityID       types.UUID         `json:"legal_entity_id"`
	SourceDocumentID    types.UUID         `json:"source_document_id"` // supplier_invoice_id
	SupplierPartyRoleID types.UUID         `json:"supplier_party_role_id"`
	OriginalAmount      types.MoneyDecimal `json:"original_amount"`
	OpenAmount          types.MoneyDecimal `json:"open_amount"`
	CurrencyCode        string             `json:"currency_code"`
	DueDate             time.Time          `json:"due_date"`
	Status              OpenItemStatus     `json:"status"`
}

// ApplyPayment decreases the payable open amount when a payment execution is confirmed.
func (poi *PayableOpenItem) ApplyPayment(appliedAmount types.MoneyDecimal) error {
	if appliedAmount.Sign() <= 0 {
		return errors.New("applied amount must be strictly positive")
	}
	if appliedAmount.Cmp(poi.OpenAmount) > 0 {
		return fmt.Errorf("over-settlement prohibited: applied payment (%s) exceeds open payable (%s)",
			appliedAmount.StringTrimmed(), poi.OpenAmount.StringTrimmed())
	}

	poi.OpenAmount = poi.OpenAmount.Sub(appliedAmount)
	if poi.OpenAmount.IsZero() {
		poi.Status = OpenItemStatusSettled
	} else {
		poi.Status = OpenItemStatusPartSettled
	}
	return nil
}
