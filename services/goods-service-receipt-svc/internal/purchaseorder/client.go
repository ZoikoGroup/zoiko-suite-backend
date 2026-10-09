// Package purchaseorder is the client for the real purchase-order-svc (AP-03).
// It verifies that a caller-supplied purchase_order_id exists, belongs to the
// caller's tenant/legal entity and is ISSUED (the only status receipts are
// allowed against), reads the PO's lines and open receipt quantities, and
// pushes received-quantity deltas back to AP-03 (which owns received/invoiced
// quantities). Fails closed throughout: any network error, timeout, or
// unexpected response rejects the caller rather than silently proceeding.
//
// Calls are service-to-service: they carry the tenant, this service's workload
// identity and the request's correlation, never the end user's principal.
package purchaseorder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/envelope"
)

// WorkloadID is this service's workload identity on outbound calls.
const WorkloadID = "goods-service-receipt-svc"

// StatusIssued is the only PO status receipts are allowed against.
const StatusIssued = "ISSUED"

// Client is the narrow interface the handler and the progress worker depend on.
type Client interface {
	// GetOpenOrder verifies purchaseOrderID against purchase-order-svc and
	// returns its summary if, and only if, it exists, belongs to
	// tenantID/legalEntityID, and is ISSUED. Any other status (draft, held,
	// cancelled, closed...) returns a *domain.PurchaseOrderNotOpenError that is
	// domain.ErrPurchaseOrderNotOpen — callers must fail closed on it and on
	// every other error.
	GetOpenOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error)

	// GetOrder is GetOpenOrder without the status check — used for read-only
	// reporting (GetReceivedToDate) where a closed order's history is still
	// legitimate to report on.
	GetOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error)

	// GetOpenQuantity returns AP-03's per-line ordered/received/open figures.
	GetOpenQuantity(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) ([]OpenQuantityLine, error)

	// PostProgress delivers one received-quantity delta to AP-03. It is
	// idempotent at AP-03 on source_ref+kind. A nil error means delivered; an
	// error wrapping domain.ErrProgressExceedsOrder is permanent; every other
	// error is transient and must be retried.
	PostProgress(ctx context.Context, p ProgressPush) error
}

// Line is one PO line as reported by AP-03.
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

// Summary is the subset of purchase-order-svc's PurchaseOrder this service
// actually needs.
type Summary struct {
	PurchaseOrderID string  `json:"purchase_order_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	Status          string  `json:"po_status"`
	TotalAmount     float64 `json:"total_amount"`
	CurrencyCode    string  `json:"currency_code"`
	Revision        int     `json:"revision"`
	Lines           []Line  `json:"lines"`
}

// Line returns the PO line with the given id, if the PO has it.
func (s *Summary) Line(lineID string) (Line, bool) {
	for _, l := range s.Lines {
		if strings.EqualFold(l.LineID, lineID) {
			return l, true
		}
	}
	return Line{}, false
}

// OpenQuantityLine is one entry of GET /v1/purchase-orders/{id}/open-quantity.
type OpenQuantityLine struct {
	LineID              string  `json:"line_id"`
	OrderedQuantity     float64 `json:"ordered_quantity"`
	ReceivedQuantity    float64 `json:"received_quantity"`
	InvoicedQuantity    float64 `json:"invoiced_quantity"`
	OpenReceiptQuantity float64 `json:"open_receipt_quantity"`
	OpenInvoiceQuantity float64 `json:"open_invoice_quantity"`
}

// ProgressPush is one received-quantity delta owed to AP-03.
type ProgressPush struct {
	TenantID        string
	LegalEntityID   string
	PurchaseOrderID string
	LineID          string
	Quantity        float64
	Amount          float64
	// SourceRef is the receipt id (or the reversal id for a reversal); with the
	// kind it is AP-03's idempotency key.
	SourceRef     string
	DeltaSign     int
	CorrelationID string
	// PrincipalID is the service identity AP-03 authorizes (PO_PROGRESS_RECORD):
	// its progress endpoint, unlike its reads, refuses a call with no principal.
	PrincipalID string
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
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *HTTPClient) GetOpenOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error) {
	s, err := c.GetOrder(ctx, tenantID, legalEntityID, purchaseOrderID)
	if err != nil {
		return nil, err
	}
	if s.Status != StatusIssued {
		return nil, &domain.PurchaseOrderNotOpenError{Status: s.Status}
	}
	return s, nil
}

func (c *HTTPClient) setHeaders(ctx context.Context, req *http.Request, tenantID, legalEntityID, correlationID string) {
	if correlationID == "" {
		if env, ok := envelope.FromContext(ctx); ok {
			correlationID = env.CorrelationID
		}
	}
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	if legalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	}
	req.Header.Set("X-Workload-Id", WorkloadID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Correlation-ID", correlationID)
}

func (c *HTTPClient) get(ctx context.Context, path, tenantID, legalEntityID string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	c.setHeaders(ctx, req, tenantID, legalEntityID, "")

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("purchase-order-svc unreachable — failing closed", zap.String("path", path), zap.Error(err))
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrPurchaseOrderNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from purchase-order-svc — failing closed",
			zap.String("path", path), zap.Int("status", resp.StatusCode))
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	return nil
}

func (c *HTTPClient) GetOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*Summary, error) {
	var s Summary
	if err := c.get(ctx, "/v1/purchase-orders/"+purchaseOrderID, tenantID, legalEntityID, &s); err != nil {
		return nil, err
	}
	if s.PurchaseOrderID == "" {
		return nil, domain.ErrPurchaseOrderNotFound
	}
	if s.TenantID != tenantID || s.LegalEntityID != legalEntityID {
		return nil, domain.ErrPurchaseOrderMismatch
	}
	return &s, nil
}

func (c *HTTPClient) GetOpenQuantity(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) ([]OpenQuantityLine, error) {
	var resp struct {
		Lines []OpenQuantityLine `json:"lines"`
	}
	if err := c.get(ctx, "/v1/purchase-orders/"+purchaseOrderID+"/open-quantity", tenantID, legalEntityID, &resp); err != nil {
		return nil, err
	}
	return resp.Lines, nil
}

func (c *HTTPClient) PostProgress(ctx context.Context, p ProgressPush) error {
	body, _ := json.Marshal(map[string]any{
		"kind":       "RECEIVED",
		"quantity":   p.Quantity,
		"amount":     p.Amount,
		"source_ref": p.SourceRef,
		"delta_sign": p.DeltaSign,
	})
	url := c.baseURL + "/v1/purchase-orders/" + p.PurchaseOrderID + "/lines/" + p.LineID + "/progress"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	c.setHeaders(ctx, req, p.TenantID, p.LegalEntityID, p.CorrelationID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "progress:RECEIVED:"+p.SourceRef)
	if p.PrincipalID != "" {
		req.Header.Set("X-Principal-Id", p.PrincipalID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ErrPurchaseOrderServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		var e struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Code == "PROGRESS_EXCEEDS_ORDER" {
			return domain.ErrProgressExceedsOrder
		}
	}
	c.log.Warn("purchase-order-svc progress push not accepted — will retry",
		zap.Int("status", resp.StatusCode), zap.String("source_ref", p.SourceRef))
	return domain.ErrPurchaseOrderServiceUnavailable
}
