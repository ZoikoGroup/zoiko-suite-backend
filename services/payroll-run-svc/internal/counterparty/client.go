// Package counterparty validates a liability authority (a tax office, insurer,
// pension or benefit provider) against counterparty-management-svc. It fails
// closed: an unreachable service is "unknown", never "valid".
package counterparty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payroll-run-svc/internal/domain"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: c}
}

type party struct {
	CounterpartyID string  `json:"counterparty_id"`
	LegalEntityID  string  `json:"legal_entity_id"`
	Status         string  `json:"status"`
	EffectiveTo    *string `json:"effective_to,omitempty"`
}

// ValidateAuthority confirms the counterparty exists, belongs to the legal entity and
// is ACTIVE. domain.ErrAuthorityInvalid means the service answered and the authority
// is not usable; domain.ErrAuthorityUnavailable means it could not be established.
func (c *Client) ValidateAuthority(ctx context.Context, tenantID, legalEntityID, counterpartyID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/counterparties/"+url.PathEscape(counterpartyID), nil)
	if err != nil {
		return domain.ErrAuthorityUnavailable
	}
	id := uuid.NewString()
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Request-Id", id)
	req.Header.Set("X-Correlation-ID", id)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "operations")
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ErrAuthorityUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return domain.ErrAuthorityInvalid
	case resp.StatusCode != http.StatusOK:
		return domain.ErrAuthorityUnavailable
	}
	var p party
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return domain.ErrAuthorityUnavailable
	}
	if p.CounterpartyID != counterpartyID || p.LegalEntityID != legalEntityID || p.Status != "ACTIVE" {
		return domain.ErrAuthorityInvalid
	}
	return nil
}
