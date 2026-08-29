// Package expenseclaim verifies a REIMBURSABLE expense claim against the
// real expense-claim-svc before it can be selected into a payment proposal.
// Fails closed. expense-claim-svc stores no total on the claim itself —
// the total is summed from its lines, the same way expense-claim-svc's own
// handler computes it; this client does the same rather than inventing a
// total field that doesn't exist upstream.
package expenseclaim

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

type Client interface {
	GetEligibleClaim(ctx context.Context, tenantID, legalEntityID, claimID string) (*Claim, error)
}

type Claim struct {
	ClaimID              string  `json:"claim_id"`
	TenantID             string  `json:"tenant_id"`
	LegalEntityID        string  `json:"legal_entity_id"`
	ClaimantPrincipalID  string  `json:"claimant_principal_id"`
	Currency             string  `json:"currency"`
	Status               string  `json:"status"`
	PaymentPreferenceRef string  `json:"payment_preference_ref"`
	TotalAmount          float64 `json:"-"`
}

type claimLine struct {
	Amount float64 `json:"amount"`
}

type claimResponse struct {
	Claim Claim       `json:"claim"`
	Lines []claimLine `json:"lines"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) GetEligibleClaim(ctx context.Context, tenantID, legalEntityID, claimID string) (*Claim, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap07/expense-claims/"+claimID, nil)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("expense-claim-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPayableNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from expense-claim-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayableServiceUnavailable
	}

	var out claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	claim := out.Claim
	if claim.ClaimID == "" || claim.TenantID != tenantID || claim.LegalEntityID != legalEntityID || claim.Status != "REIMBURSABLE" {
		return nil, domain.ErrPayableNotEligible
	}
	var total float64
	for _, l := range out.Lines {
		total += l.Amount
	}
	claim.TotalAmount = total
	return &claim, nil
}
