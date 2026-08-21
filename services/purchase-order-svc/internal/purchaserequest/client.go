// Package purchaserequest provides a client for verifying, against the real
// purchase-request-svc, that a caller-supplied purchase_request_id genuinely
// exists, belongs to the caller's own tenant/legal entity, and is APPROVED —
// before letting it seed a PurchaseOrder.
//
// This deliberately follows accounts-receivable-svc's/
// bank-reconciliation-svc's "verify against the real upstream record"
// pattern rather than trusting a caller-supplied foreign ID outright (the
// 2026-07-23 platform audit found the opposite anti-pattern —
// corporate-actions-svc executing a corporate action against an unverified
// resolution_id — and flagged it as a real gap). Fail-closed throughout:
// any network error, timeout, or unexpected response rejects the Issue
// request rather than silently proceeding.
package purchaserequest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// GetApprovedRequest verifies requestID against purchase-request-svc and
	// returns its summary if, and only if, it exists, belongs to tenantID,
	// and is APPROVED. Any other outcome is a typed domain error — callers
	// must fail closed on all of them.
	GetApprovedRequest(ctx context.Context, tenantID, legalEntityID, requestID string) (*Summary, error)
}

// Summary is the subset of purchase-request-svc's PurchaseRequest this
// service actually needs.
type Summary struct {
	RequestID     string  `json:"request_id"`
	TenantID      string  `json:"tenant_id"`
	LegalEntityID string  `json:"legal_entity_id"`
	Status        string  `json:"status"`
	Amount        float64 `json:"amount"`
	CurrencyCode  string  `json:"currency_code"`
}

// HTTPClient implements Client against a real purchase-request-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://purchase-request-svc:8100" (no trailing slash).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *HTTPClient) GetApprovedRequest(ctx context.Context, tenantID, legalEntityID, requestID string) (*Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/purchase-requests/"+requestID, nil)
	if err != nil {
		return nil, domain.ErrPurchaseRequestServiceUnavailable
	}
	// purchase-request-svc's GetRequest reads tenant scope from this header
	// via its own TenantContext middleware, not a query param — see that
	// service's internal/middleware/tenant.go.
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("purchase-request-svc unreachable — failing closed",
			zap.String("request_id", requestID), zap.Error(err))
		return nil, domain.ErrPurchaseRequestServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPurchaseRequestNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from purchase-request-svc — failing closed",
			zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPurchaseRequestServiceUnavailable
	}

	var s Summary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, domain.ErrPurchaseRequestServiceUnavailable
	}
	// GetRequest returns 200 with an empty body's zero-value struct in no
	// known case, but a defensive check costs nothing: an empty
	// RequestID means the upstream response didn't actually contain a
	// request (e.g. tenant scope mismatch resolved server-side to "not
	// found" rather than a 404).
	if s.RequestID == "" {
		return nil, domain.ErrPurchaseRequestNotFound
	}
	if s.TenantID != tenantID || s.LegalEntityID != legalEntityID {
		return nil, domain.ErrPurchaseRequestMismatch
	}
	if s.Status != "APPROVED" {
		return nil, domain.ErrPurchaseRequestNotApproved
	}
	return &s, nil
}
