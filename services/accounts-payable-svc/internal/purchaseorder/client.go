// Package purchaseorder is accounts-payable-svc's client for purchase-order-svc
// (AP-03).
//
// Three jobs, all fail-closed:
//
//   - Verify: at invoice capture/validation, the referenced PO must exist, belong
//     to the same legal entity and be ISSUED (contract #2: receipts and invoices
//     are allowed only while po_status == "ISSUED").
//   - GetOrder / OpenQuantity: the frozen PO revision, lines and open quantities
//     AP-06 matches against (contract #2: GET /v1/purchase-orders/{id} adds
//     "revision" and "lines"; GET .../open-quantity).
//   - ReportProgress: AP-05/06 push invoiced quantities back to AP-03
//     (POST /v1/purchase-orders/{id}/lines/{line_id}/progress, idempotent on
//     source_ref + kind).
package purchaseorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

// Line is one PO line (contract #2).
type Line struct {
	LineID      string  `json:"line_id"`
	LineNumber  int     `json:"line_number"`
	ItemRef     string  `json:"item_ref"`
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	UOM         string  `json:"uom"`
	LineAmount  float64 `json:"line_amount"`
}

type PurchaseOrder struct {
	PurchaseOrderID string  `json:"purchase_order_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	VendorProfileID string  `json:"vendor_profile_id"`
	POStatus        string  `json:"po_status"`
	TotalAmount     float64 `json:"total_amount"`
	CurrencyCode    string  `json:"currency_code"`

	// Revision and Lines are contract #2 additions. HasRevision/HasLines record
	// whether the response actually carried them, so a PO service that has not
	// shipped them yet yields INCOMPLETE matches instead of an empty "match".
	Revision    int    `json:"revision"`
	Lines       []Line `json:"lines"`
	HasRevision bool   `json:"-"`
	HasLines    bool   `json:"-"`
}

// LineProgress is one line of the open-quantity read.
type LineProgress struct {
	LineID              string  `json:"line_id"`
	OrderedQuantity     float64 `json:"ordered_quantity"`
	ReceivedQuantity    float64 `json:"received_quantity"`
	InvoicedQuantity    float64 `json:"invoiced_quantity"`
	OpenReceiptQuantity float64 `json:"open_receipt_quantity"`
	OpenInvoiceQuantity float64 `json:"open_invoice_quantity"`
}

// ProgressReport is the body of POST .../lines/{line_id}/progress.
type ProgressReport struct {
	Kind      string  `json:"kind"` // INVOICED
	Quantity  float64 `json:"quantity"`
	Amount    float64 `json:"amount"`
	SourceRef string  `json:"source_ref"`
	DeltaSign int     `json:"delta_sign"` // 1 | -1
}

// Reader is what the matching module depends on.
type Reader interface {
	GetOrder(ctx context.Context, caller svcclient.Caller, purchaseOrderID string) (*PurchaseOrder, error)
	OpenQuantity(ctx context.Context, caller svcclient.Caller, purchaseOrderID string) ([]LineProgress, error)
	ReportProgress(ctx context.Context, caller svcclient.Caller, purchaseOrderID, lineID string, r ProgressReport) error
}

type Client struct {
	c *svcclient.Client
}

func NewClient(baseURL string) *Client { return &Client{c: svcclient.New(baseURL)} }

// Verify validates AP-05's PO reference. It returns ErrPurchaseOrderUnknown,
// ErrPurchaseOrderClosed, ErrPurchaseOrderNotIssued or (wrapping)
// ErrPurchaseOrderUnverifiable.
func (c *Client) Verify(ctx context.Context, tenantID, legalEntityID, correlationID, purchaseOrderID string) (*PurchaseOrder, error) {
	if purchaseOrderID == "" {
		return nil, nil
	}
	po, err := c.GetOrder(ctx, svcclient.Caller{TenantID: tenantID, CorrelationID: correlationID}, purchaseOrderID)
	if err != nil {
		if errors.Is(err, domain.ErrPurchaseOrderUnknown) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrPurchaseOrderUnverifiable, err)
	}
	if po.LegalEntityID != "" && legalEntityID != "" && po.LegalEntityID != legalEntityID {
		return nil, domain.ErrPurchaseOrderUnknown
	}
	status := strings.ToUpper(strings.TrimSpace(po.POStatus))
	switch {
	case closedStatus(status):
		return nil, domain.ErrPurchaseOrderClosed
	case status != "ISSUED":
		return nil, domain.ErrPurchaseOrderNotIssued
	}
	return po, nil
}

// GetOrder fetches the PO as AP-03 currently states it. A 404 is
// ErrPurchaseOrderUnknown; any other failure is svcclient.ErrUnavailable.
func (c *Client) GetOrder(ctx context.Context, caller svcclient.Caller, purchaseOrderID string) (*PurchaseOrder, error) {
	status, body, err := c.c.Do(ctx, caller, "GET", "/v1/purchase-orders/"+url.PathEscape(purchaseOrderID), nil)
	if err != nil {
		return nil, svcclient.ErrUnavailable
	}
	switch status {
	case 200:
	case 404:
		return nil, domain.ErrPurchaseOrderUnknown
	default:
		return nil, svcclient.ErrUnavailable
	}
	var po PurchaseOrder
	if err := json.Unmarshal(body, &po); err != nil {
		return nil, svcclient.ErrUnavailable
	}
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(body, &probe)
	_, po.HasRevision = probe["revision"]
	_, po.HasLines = probe["lines"]
	return &po, nil
}

// OpenQuantity reads AP-03's per-line ordered/received/invoiced quantities.
func (c *Client) OpenQuantity(ctx context.Context, caller svcclient.Caller, purchaseOrderID string) ([]LineProgress, error) {
	status, body, err := c.c.Do(ctx, caller, "GET", "/v1/purchase-orders/"+url.PathEscape(purchaseOrderID)+"/open-quantity", nil)
	if err != nil || status != 200 {
		return nil, svcclient.ErrUnavailable
	}
	var out struct {
		Lines []LineProgress `json:"lines"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, svcclient.ErrUnavailable
	}
	return out.Lines, nil
}

// ReportProgress pushes invoiced quantity to AP-03. Idempotent on
// source_ref + kind at the receiving side.
func (c *Client) ReportProgress(ctx context.Context, caller svcclient.Caller, purchaseOrderID, lineID string, r ProgressReport) error {
	path := "/v1/purchase-orders/" + url.PathEscape(purchaseOrderID) + "/lines/" + url.PathEscape(lineID) + "/progress"
	status, _, err := c.c.Do(ctx, caller, "POST", path, r)
	if err != nil {
		return svcclient.ErrUnavailable
	}
	if status == 200 || status == 201 || status == 202 || status == 204 {
		return nil
	}
	return svcclient.ErrUnavailable
}

func closedStatus(s string) bool {
	switch s {
	case "CLOSED", "CANCELLED", "CANCELED", "VOIDED", "SUPERSEDED":
		return true
	default:
		return false
	}
}
