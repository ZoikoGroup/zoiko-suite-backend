// Package supplier asks supplier-financial-profile-svc (AP-01) whether a
// supplier may receive NEW purchase orders.
//
// The contract (ZS-SVC-D-001 shared contract #1) is
//
//	GET /ap01/supplier-financial-profiles/eligibility?legal_entity_id=&supplier_ref=
//
// answering whether the profile is ACTIVE and not on hold. Everything here fails
// closed: an unreachable AP-01 or an unreadable answer is never treated as "the
// supplier is fine".
package supplier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
)

// Eligibility is AP-01's answer.
type Eligibility struct {
	ProfileID                 string `json:"profile_id"`
	SupplierRef               string `json:"supplier_ref"`
	LegalEntityID             string `json:"legal_entity_id"`
	Status                    string `json:"status"`
	Version                   int    `json:"version"`
	IsOnHold                  bool   `json:"is_on_hold"`
	EligibleForNewCommitments bool   `json:"eligible_for_new_commitments"`
	Reason                    string `json:"reason"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	// Eligibility returns AP-01's verdict, domain.ErrSupplierUnknown when no
	// profile exists, or domain.ErrSupplierServiceUnavailable.
	Eligibility(ctx context.Context, tenantID, principalID, legalEntityID, supplierRef string) (*Eligibility, error)
}

// HTTPClient implements Client.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}, log: log}
}

func (c *HTTPClient) Eligibility(ctx context.Context, tenantID, principalID, legalEntityID, supplierRef string) (*Eligibility, error) {
	q := url.Values{"legal_entity_id": {legalEntityID}, "supplier_ref": {supplierRef}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap01/supplier-financial-profiles/eligibility?"+q.Encode(), nil)
	if err != nil {
		return nil, domain.ErrSupplierServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("supplier-financial-profile-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrSupplierServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, domain.ErrSupplierUnknown
	default:
		c.log.Error("unexpected response from supplier-financial-profile-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrSupplierServiceUnavailable
	}
	var out Eligibility
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrSupplierServiceUnavailable
	}
	return &out, nil
}
