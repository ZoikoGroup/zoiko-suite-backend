// Package procurementcase verifies, against procurement-workflow-svc, that a
// procurement case really was approved — the approval basis for the legacy
// direct-issue endpoint (POST /v1/purchase-orders), which has no requisition.
//
// procurement-workflow-svc passes its own case_id as the order's correlation_id
// (see its PurchaseOrderClient), so the case is addressable from the order
// request. The case must exist in the same tenant and legal entity, be APPROVED
// (or COMPLETED, for a replay), carry an approver, and match the order's amount
// and currency. Fail-closed throughout: if the case cannot be read, the order is
// not issued.
package procurementcase

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
)

// Case is the subset of procurement-workflow-svc's ProcurementCase used here.
type Case struct {
	CaseID                string  `json:"case_id"`
	TenantID              string  `json:"tenant_id"`
	LegalEntityID         string  `json:"legal_entity_id"`
	Amount                float64 `json:"amount"`
	CurrencyCode          string  `json:"currency_code"`
	Status                string  `json:"status"`
	ApprovedByPrincipalID *string `json:"approved_by_principal_id"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	// VerifyApproved returns the case if, and only if, it is approved and matches
	// the order. Errors are typed domain errors; callers fail closed on all.
	VerifyApproved(ctx context.Context, tenantID, principalID, legalEntityID, caseID string, amount float64, currency string) (*Case, error)
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

func (c *HTTPClient) VerifyApproved(ctx context.Context, tenantID, principalID, legalEntityID, caseID string, amount float64, currency string) (*Case, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/procurement-cases/"+url.PathEscape(caseID), nil)
	if err != nil {
		return nil, domain.ErrProcurementCaseServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("procurement-workflow-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrProcurementCaseServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, domain.ErrProcurementCaseNotApproved
	default:
		c.log.Error("unexpected response from procurement-workflow-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrProcurementCaseServiceUnavailable
	}
	var pc Case
	if err := json.NewDecoder(resp.Body).Decode(&pc); err != nil {
		return nil, domain.ErrProcurementCaseServiceUnavailable
	}
	if pc.CaseID == "" || !strings.EqualFold(pc.TenantID, tenantID) || !strings.EqualFold(pc.LegalEntityID, legalEntityID) {
		return nil, domain.ErrProcurementCaseMismatch
	}
	if (pc.Status != "APPROVED" && pc.Status != "COMPLETED") || pc.ApprovedByPrincipalID == nil || *pc.ApprovedByPrincipalID == "" {
		return nil, domain.ErrProcurementCaseNotApproved
	}
	if math.Abs(pc.Amount-amount) > 0.005 || !strings.EqualFold(pc.CurrencyCode, currency) {
		return nil, domain.ErrProcurementCaseMismatch
	}
	return &pc, nil
}
