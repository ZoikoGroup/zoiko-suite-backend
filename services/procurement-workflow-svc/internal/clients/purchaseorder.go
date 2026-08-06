package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PurchaseOrderClient calls purchase-order-svc's real POST /v1/purchase-orders
// endpoint synchronously to issue the actual order once a procurement case is
// approved.
type PurchaseOrderClient struct {
	baseURL string
	http    *http.Client
}

func NewPurchaseOrderClient(baseURL string) *PurchaseOrderClient {
	return &PurchaseOrderClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

type IssuedOrder struct {
	PurchaseOrderID string `json:"purchase_order_id"`
	PONumber        string `json:"po_number"`
}

// IssueOrder passes the procurement case's own case_id as the correlation_id
// — a globally unique value, so retrying order issuance for the same case
// replays purchase-order-svc's own idempotent insert rather than issuing a
// second order.
func (c *PurchaseOrderClient) IssueOrder(ctx context.Context, tenantID, principalID, legalEntityID, caseID string, vendorProfileID *string, totalAmount float64, currencyCode string) (*IssuedOrder, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"tenant_id":         tenantID,
		"legal_entity_id":   legalEntityID,
		"vendor_profile_id": vendorProfileID,
		"total_amount":      totalAmount,
		"currency_code":     currencyCode,
		"correlation_id":    caseID,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/purchase-orders", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("purchase-order-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("purchase-order-svc returned %d for POST /v1/purchase-orders", resp.StatusCode)
	}

	var order IssuedOrder
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		return nil, fmt.Errorf("decode purchase-order-svc response: %w", err)
	}
	return &order, nil
}
