// Package purchaseorder validates AP-05's "PO refs" input against
// purchase-order-svc.
//
// §9.F lists PO and receipt references among a supplier invoice's required
// inputs, and names "duplicate detection; supplier verification" among what the
// service must resolve rather than accept. A purchase order is the commitment an
// invoice is claimed against: recording an invoice against a PO that does not
// exist, belongs to another entity, or has been closed is how an unauthorised
// spend enters the payables ledger looking authorised.
//
// This is not AP-06 Invoice Matching, which does not exist. It validates the
// reference; it does not compare quantities or prices.
package purchaseorder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// PurchaseOrder is the subset of purchase-order-svc's response this package
// reads. Declared narrowly so an unrelated change to that service's shape
// cannot break decoding here.
type PurchaseOrder struct {
	PurchaseOrderID string  `json:"purchase_order_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	VendorProfileID string  `json:"vendor_profile_id"`
	POStatus        string  `json:"po_status"`
	TotalAmount     float64 `json:"total_amount"`
	CurrencyCode    string  `json:"currency_code"`
}

// Client reads purchase orders from purchase-order-svc.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. The timeout is short because this sits in front of
// invoice intake: a slow purchase-order service must surface as a fast
// fail-closed refusal rather than as latency on every invoice keyed.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Verify checks that the referenced purchase order exists, belongs to
// legalEntityID, and is still open to invoicing.
//
// Returns the PO so the caller can record its supplier for later comparison.
// An empty purchaseOrderID passes and returns nil: §9.F treats the PO reference
// as one of a pair with the receipt reference, and an invoice with neither is a
// non-PO invoice, which is ordinary.
func (c *Client) Verify(ctx context.Context, tenantID, legalEntityID, correlationID, purchaseOrderID string) (*PurchaseOrder, error) {
	if purchaseOrderID == "" {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/purchase-orders/%s", c.baseURL, purchaseOrderID), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrPurchaseOrderUnverifiable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Accept", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrPurchaseOrderUnverifiable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, domain.ErrPurchaseOrderUnknown
	case resp.StatusCode == http.StatusOK:
		// fall through
	default:
		return nil, fmt.Errorf("%w: purchase-order-svc returned %d", domain.ErrPurchaseOrderUnverifiable, resp.StatusCode)
	}

	var po PurchaseOrder
	if err := json.NewDecoder(resp.Body).Decode(&po); err != nil {
		return nil, fmt.Errorf("%w: malformed purchase order response: %v", domain.ErrPurchaseOrderUnverifiable, err)
	}

	// A PO belonging to another legal entity reads the same as one that does
	// not exist. Distinguishing them would confirm to a caller that a PO it has
	// no relationship with is real.
	if po.LegalEntityID != "" && legalEntityID != "" && po.LegalEntityID != legalEntityID {
		return nil, domain.ErrPurchaseOrderUnknown
	}

	if closedStatus(po.POStatus) {
		return nil, domain.ErrPurchaseOrderClosed
	}
	return &po, nil
}

// closedStatus reports whether a PO status means "no longer accepting
// invoices".
//
// Deny-listed rather than allow-listed, deliberately and unlike the tenant
// lifecycle check in the envelope resolver. There the risk of admitting an
// unconsidered state was the whole point; here the opposite risk dominates: a
// status purchase-order-svc adds later would, under an allow-list, silently
// start refusing every invoice against every PO in that state. Refusing to pay
// suppliers is the worse failure, and an unknown status is visible in the data
// rather than swallowed.
func closedStatus(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CLOSED", "CANCELLED", "CANCELED", "VOIDED", "SUPERSEDED":
		return true
	default:
		return false
	}
}
