// Package purchaseorder asks purchase-order-svc (AP-03) to create the purchase
// order a requisition converts into.
//
// The PO is created as a DRAFT through POST /v1/purchase-orders/draft, keyed by
// a correlation_id derived from the requisition id, so a retried conversion
// (e.g. the PO was created but recording the link failed) resolves to the SAME
// order rather than a duplicate. Everything downstream of the draft — PO
// approval, supplier-eligibility check, issue — stays AP-03's own governance.
package purchaseorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrUnavailable: purchase-order-svc could not be reached or answered 5xx.
var ErrUnavailable = errors.New("purchase-order-svc unavailable")

// RefusedError is a 4xx answer from purchase-order-svc (the PO was not created).
type RefusedError struct {
	Status int
	Code   string
	Detail string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("purchase-order-svc refused (%d %s): %s", e.Status, e.Code, e.Detail)
}

// Line is one PO line derived from a requisition line.
type Line struct {
	ItemRef     string  `json:"item_ref"`
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	UOM         string  `json:"uom"`
	LineAmount  float64 `json:"line_amount"`
}

// DraftInput describes the PO to create.
type DraftInput struct {
	TenantID          string
	PrincipalID       string
	LegalEntityID     string
	PurchaseRequestID string
	SupplierRef       string
	CurrencyCode      string
	CorrelationID     string
	Lines             []Line
}

// Created is the PO that now exists for the requisition.
type Created struct {
	PurchaseOrderID string `json:"purchase_order_id"`
	PONumber        string `json:"po_number"`
	Status          string `json:"po_status"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	CreateDraft(ctx context.Context, in DraftInput) (*Created, error)
}

// HTTPClient implements Client.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient builds a client for baseURL (e.g. http://purchase-order-svc:8129).
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *HTTPClient) CreateDraft(ctx context.Context, in DraftInput) (*Created, error) {
	body, _ := json.Marshal(map[string]any{
		"legal_entity_id":     in.LegalEntityID,
		"purchase_request_id": in.PurchaseRequestID,
		"supplier_ref":        in.SupplierRef,
		"currency_code":       in.CurrencyCode,
		"correlation_id":      in.CorrelationID,
		"lines":               in.Lines,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/purchase-orders/draft", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", in.TenantID)
	req.Header.Set("X-Principal-Id", in.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", in.LegalEntityID)
	req.Header.Set("X-Correlation-ID", in.CorrelationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Idempotency-Key", "convert:"+in.CorrelationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		var out Created
		if err := json.Unmarshal(raw, &out); err != nil || out.PurchaseOrderID == "" {
			return nil, fmt.Errorf("%w: malformed purchase order response", ErrUnavailable)
		}
		return &out, nil
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: purchase-order-svc returned %d", ErrUnavailable, resp.StatusCode)
	default:
		var e struct {
			Error  string `json:"error"`
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, &RefusedError{Status: resp.StatusCode, Code: e.Code, Detail: strings.TrimSpace(e.Error + " " + e.Detail)}
	}
}
