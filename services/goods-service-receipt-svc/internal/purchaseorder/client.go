// Package purchaseorder provides a client for verifying, against the real
// purchase-order-svc, that a caller-supplied purchase_order_id genuinely
// exists, belongs to the caller's own tenant/legal entity, and is still
// open — before letting a receipt be created or confirmed against it. Fails
// closed throughout: any network error, timeout, or unexpected response
// rejects the caller rather than silently proceeding.
//
// purchase-order-svc has no PO-line or quantity model and only two statuses
// (ISSUED, CLOSED — no CANCELLED). See internal/domain's package doc for why
// this deliberately narrows the spec's own "PO line/quantity" contract to an
// aggregate amount + open/closed check against real upstream data.
package purchaseorder

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// GetOpenOrder verifies purchaseOrderID against purchase-order-svc and
	// returns its summary if, and only if, it exists, belongs to
	// tenantID/legalEntityID, and is ISSUED (open). A CLOSED order returns
	// domain.ErrPurchaseOrderNotOpen — callers must fail closed on it and
	// every other error.
	GetOpenOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error)

	// GetOrder is GetOpenOrder without the open/closed check — used for
	// read-only reporting (GetReceivedToDate) where a closed order's
	// history is still legitimate to report on.
	GetOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error)
}

// Summary is the subset of purchase-order-svc's PurchaseOrder this service
// actually needs.
type Summary struct {
	PurchaseOrderID string  `json:"purchase_order_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	Status          string  `json:"po_status"`
	TotalAmount     float64 `json:"total_amount"`
	CurrencyCode    string  `json:"currency_code"`
}

// HTTPClient implements Client against a real purchase-order-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://purchase-order-svc:8129" (no trailing slash).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *HTTPClient) GetOpenOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error) {
	s, err := c.GetOrder(ctx, tenantID, legalEntityID, purchaseOrderID)
	if err != nil {
		return nil, err
	}
	if s.Status != "ISSUED" {
		return nil, domain.ErrPurchaseOrderNotOpen
	}
	return s, nil
}

func (c *HTTPClient) GetOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/purchase-orders/"+purchaseOrderID, nil)
	if err != nil {
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("purchase-order-svc unreachable — failing closed",
			zap.String("purchase_order_id", purchaseOrderID), zap.Error(err))
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPurchaseOrderNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from purchase-order-svc — failing closed",
			zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}

	var s Summary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}
	if s.PurchaseOrderID == "" {
		return nil, domain.ErrPurchaseOrderNotFound
	}
	if s.TenantID != tenantID || s.LegalEntityID != legalEntityID {
		return nil, domain.ErrPurchaseOrderMismatch
	}
	return &s, nil
}
