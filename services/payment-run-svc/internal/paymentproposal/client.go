// Package paymentproposal is a read-only HTTP client to payment-proposal-svc
// (AP-09). AP-11 reads the frozen, authorized proposal's items at CreateRun
// so it can build one bank instruction per payee, each carrying that
// payee's own net total — never the whole proposal to one payee. Fails
// closed throughout.
package paymentproposal

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
)

type Client interface {
	// GetFrozenProposal returns the proposal, its active items and its
	// current fingerprint, as AP-09 reports them.
	GetFrozenProposal(ctx context.Context, tenantID, principalID, proposalID string) (*Proposal, error)
}

// Proposal is the subset of AP-09's PaymentProposal (PascalCase wire shape,
// no json tags on AP-09's side) this service needs.
type Proposal struct {
	ProposalID           string    `json:"ProposalID"`
	LegalEntityID        string    `json:"LegalEntityID"`
	PayingBankAccountRef string    `json:"PayingBankAccountRef"`
	Currency             string    `json:"Currency"`
	PaymentDate          time.Time `json:"PaymentDate"`
	PaymentMethod        string    `json:"PaymentMethod"`
	Status               string    `json:"Status"`
	NetAmount            float64   `json:"NetAmount"`

	Items       []Item `json:"-"`
	Fingerprint string `json:"-"`
}

type Item struct {
	PayableSource     string  `json:"PayableSource"`
	PayableID         string  `json:"PayableID"`
	PayeeRef          string  `json:"PayeeRef"`
	GrossAmount       float64 `json:"GrossAmount"`
	WithholdingAmount float64 `json:"WithholdingAmount"`
	NetAmount         float64 `json:"NetAmount"`
	Currency          string  `json:"Currency"`
	IsActive          bool    `json:"IsActive"`
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

func (c *HTTPClient) get(ctx context.Context, path, tenantID, principalID string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return domain.ErrProposalServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-proposal-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrProposalServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrAuthorizedSubjectMismatch
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payment-proposal-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrProposalServiceUnavailable
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return domain.ErrProposalServiceUnavailable
	}
	return nil
}

func (c *HTTPClient) GetFrozenProposal(ctx context.Context, tenantID, principalID, proposalID string) (*Proposal, error) {
	var body getProposalResponse
	if err := c.get(ctx, "/ap09/proposals/"+proposalID, tenantID, principalID, &body); err != nil {
		return nil, err
	}
	var fp fingerprintResponse
	if err := c.get(ctx, "/ap09/proposals/"+proposalID+"/fingerprint", tenantID, principalID, &fp); err != nil {
		return nil, err
	}

	p := body.Proposal
	for _, it := range body.Items {
		if it.IsActive {
			p.Items = append(p.Items, it)
		}
	}
	p.Fingerprint = fp.Fingerprint
	return &p, nil
}
