// Package payeeidentity is a real HTTP client to payee-banking-identity-svc
// (ORG-10) — closes ORG-10's own named dependency ("AP-10 fingerprints
// active version"). Fails closed on a genuine service error, but a payee
// with no ORG-10 coverage yet (ErrNoActiveDestination) is a real, expected
// absence, not a failure — see internal/domain's package doc.
//
// GetActiveDestination did not actually close that dependency until its
// X-Principal-Id forwarding was fixed: payee-banking-identity-svc requires
// that header unconditionally, so every call 401ed, every payee resolved
// as "no ORG-10 coverage", and the destination-pinning re-verification
// gate it feeds (verifyStillEligible) was permanently dead code.
package payeeidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

// Destination is the subset of ORG-10's own PayeeDestination (PascalCase
// wire shape, no json tags on ORG-10's side) this service needs.
type Destination struct {
	DestinationID string `json:"DestinationID"`
	LegalEntityID string `json:"LegalEntityID"`
	Status        string `json:"Status"`
}

type Client interface {
	// GetActiveDestination returns ORG-10's current active destination for
	// partyRef, or domain.ErrNoActiveDestination if ORG-10 has none on file
	// — a real, expected absence for a payee ORG-10 hasn't onboarded yet.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/org10/parties/"+partyRef+"/active?"+q.Encode(), nil)
	if err != nil {
		return nil, domain.ErrPayeeDestinationServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	// payee-banking-identity-svc's GetActivePayeeDestination unconditionally
	// requires X-Principal-Id (401 without it) — this was never sent, which
	// silently neutered this client's only real reason to exist: the
	// destination-pinning re-verification it feeds (verifyStillEligible)
	// permanently no-op'd for every tenant, every authorization, because
	// DestinationID could never be populated in the first place. Found
	// while auditing identity-header forwarding across this service's
	// cross-service clients — same bug shape as evidence-manifest-svc's
	// missing X-Principal-Id forwarding to workflow-history-svc.
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payee-banking-identity-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayeeDestinationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrNoActiveDestination
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payee-banking-identity-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayeeDestinationServiceUnavailable
	}

	var d Destination
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, domain.ErrPayeeDestinationServiceUnavailable
	}
	if d.DestinationID == "" || d.LegalEntityID != legalEntityID {
		return nil, domain.ErrNoActiveDestination
	}
	return &d, nil
}
