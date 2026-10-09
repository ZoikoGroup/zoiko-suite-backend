// Package payeeidentity is a real HTTP client to payee-banking-identity-svc
// (ORG-10): the controlled source of a reimbursement payee. A claim is
// payable only when ORG-10 holds an ACTIVE destination for the claimant's
// payee party reference in the claim's legal entity. The absence of one is
// a real, expected outcome (domain.ErrNoControlledPayee), distinct from the
// service being unreachable (domain.ErrPayeeServiceUnavailable).
package payeeidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
)

// Destination is the subset of ORG-10's PayeeDestination (PascalCase wire
// shape, no json tags on ORG-10's side) this service needs.
type Destination struct {
	DestinationID string `json:"DestinationID"`
	LegalEntityID string `json:"LegalEntityID"`
	Status        string `json:"Status"`
	Currency      string `json:"Currency"`
}

type Client interface {
	// GetActiveDestination returns ORG-10's active destination for partyRef
	// or domain.ErrNoControlledPayee.
	GetActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, partyRef string) (*Destination, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, partyRef string) (*Destination, error) {
	q := url.Values{"scope": {"DEFAULT"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/org10/parties/"+url.PathEscape(partyRef)+"/active?"+q.Encode(), nil)
	if err != nil {
		return nil, domain.ErrPayeeServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	// payee-banking-identity-svc rejects a request with no principal.
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payee-banking-identity-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayeeServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrNoControlledPayee
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payee-banking-identity-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayeeServiceUnavailable
	}
	var d Destination
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, domain.ErrPayeeServiceUnavailable
	}
	if d.DestinationID == "" || d.LegalEntityID != legalEntityID || d.Status != "ACTIVE" {
		return nil, domain.ErrNoControlledPayee
	}
	return &d, nil
}
