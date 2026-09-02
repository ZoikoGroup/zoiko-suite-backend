// Package counterparty is a real HTTP client to counterparty-management-svc
// (ORG-07) — verifies a proposed payee destination's named party genuinely
// exists before this service ever creates a candidate against it. Fails
// closed.
package counterparty

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

// Counterparty is the subset of counterparty-management-svc's own
// Counterparty (snake_case wire shape — this peer predates the
// PascalCase-no-tags convention used elsewhere in this session) this
// service needs.
type Counterparty struct {
	CounterpartyID   string `json:"counterparty_id"`
	TenantID         string `json:"tenant_id"`
	LegalEntityID    string `json:"legal_entity_id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	ComplianceStatus string `json:"compliance_status"`
}

type Client interface {
	// GetParty confirms partyRef exists and belongs to legalEntityID,
	// surfacing its real ComplianceStatus as this service's own
	// "party status" server-resolved-context signal.
	GetParty(ctx context.Context, tenantID, legalEntityID, partyRef string) (*Counterparty, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetParty(ctx context.Context, tenantID, legalEntityID, partyRef string) (*Counterparty, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/counterparties/"+partyRef, nil)
	if err != nil {
		return nil, domain.ErrPartyServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("counterparty-management-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPartyServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPartyNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from counterparty-management-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPartyServiceUnavailable
	}

	var cp Counterparty
	if err := json.NewDecoder(resp.Body).Decode(&cp); err != nil {
		return nil, domain.ErrPartyServiceUnavailable
	}
	if cp.CounterpartyID == "" || cp.LegalEntityID != legalEntityID {
		return nil, domain.ErrPartyNotFound
	}
	return &cp, nil
}
