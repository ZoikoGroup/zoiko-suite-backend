// Package employeemaster verifies a claimant against the real
// employee-master-svc before a claim can be created against them. Fails
// closed: any network error, timeout, unexpected response, or non-ACTIVE
// status rejects the caller.
package employeemaster

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
)

type Client interface {
	// VerifyActiveClaimant confirms claimantID exists, belongs to
	// legalEntityID, and has Status == "ACTIVE".
	VerifyActiveClaimant(ctx context.Context, actingPrincipalID, legalEntityID, claimantID string) error
}

type employee struct {
	EmployeeID    string `json:"employee_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) VerifyActiveClaimant(ctx context.Context, actingPrincipalID, legalEntityID, claimantID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/employees/"+claimantID, nil)
	if err != nil {
		return domain.ErrClaimantServiceUnavailable
	}
	req.Header.Set("X-Principal-Id", actingPrincipalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("employee-master-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrClaimantServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrClaimantNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from employee-master-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrClaimantServiceUnavailable
	}

	var e employee
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return domain.ErrClaimantServiceUnavailable
	}
	if e.EmployeeID == "" || e.LegalEntityID != legalEntityID || e.Status != "ACTIVE" {
		return domain.ErrClaimantNotEligible
	}
	return nil
}
