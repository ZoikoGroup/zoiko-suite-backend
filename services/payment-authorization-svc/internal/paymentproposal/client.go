// Package paymentproposal is a real HTTP client to payment-proposal-svc
// (AP-09) — the frozen proposal this service authorizes, and the only
// source of its own subject fingerprint. Fails closed.
package paymentproposal

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

type Client interface {
	// GetProposal fetches proposalID, verifying only that it belongs to
	// tenantID — there is no legal_entity_id to check against yet at
	// RequestPaymentAuthorization time (this call is what discovers it).
	// The caller is responsible for checking Status == "FROZEN" and for
	// authorizing against the returned LegalEntityID.
	GetProposal(ctx context.Context, tenantID, proposalID string) (*Proposal, error)
	// GetFingerprint returns the proposal's current subject fingerprint —
	// callable at any status, used both at request time and again as a
	// live re-check at approval/consumption time.
	GetFingerprint(ctx context.Context, tenantID, proposalID string) (string, error)
}

// AP-09's own domain types carry no json tags, so its wire shape is their
// exact Go (PascalCase) field names — matched here field-for-field.
type Item struct {
	PayableSource   string     `json:"PayableSource"`
	PayableID       string     `json:"PayableID"`
	PayeeRef        string     `json:"PayeeRef"`
	PayeeSnapshotAt *time.Time `json:"PayeeSnapshotAt"`
}

type Proposal struct {
	ProposalID           string  `json:"ProposalID"`
	TenantID             *string `json:"TenantID"`
	LegalEntityID        string  `json:"LegalEntityID"`
	Status               string  `json:"Status"`
	NetAmount            float64 `json:"NetAmount"`
	Currency             string  `json:"Currency"`
	CreatedByPrincipalID string  `json:"CreatedByPrincipalID"`
	Items                []Item  `json:"-"`
}

type getProposalResponse struct {
	Proposal Proposal `json:"proposal"`
	Items    []Item   `json:"items"`
}

type fingerprintResponse struct {
	Fingerprint string `json:"fingerprint"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetProposal(ctx context.Context, tenantID, proposalID string) (*Proposal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap09/proposals/"+proposalID, nil)
	if err != nil {
		return nil, domain.ErrProposalServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-proposal-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrProposalServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrProposalNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payment-proposal-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrProposalServiceUnavailable
	}

	var out getProposalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrProposalServiceUnavailable
	}
	p := out.Proposal
	p.Items = out.Items
	if p.ProposalID == "" || (p.TenantID != nil && *p.TenantID != tenantID) {
		return nil, domain.ErrProposalNotEligible
	}
	return &p, nil
}

func (c *HTTPClient) GetFingerprint(ctx context.Context, tenantID, proposalID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap09/proposals/"+proposalID+"/fingerprint", nil)
	if err != nil {
		return "", domain.ErrProposalServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-proposal-svc unreachable — failing closed", zap.Error(err))
		return "", domain.ErrProposalServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response fetching fingerprint — failing closed", zap.Int("status", resp.StatusCode))
		return "", domain.ErrProposalServiceUnavailable
	}

	var out fingerprintResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Fingerprint == "" {
		return "", domain.ErrProposalServiceUnavailable
	}
	return out.Fingerprint, nil
}
