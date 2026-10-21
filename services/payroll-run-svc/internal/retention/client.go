// Package retention asks retention-registry-svc whether a class of record may be disposed of. It fails closed:
// an answer that cannot be had is "cannot answer", never "no hold".
package retention

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/payroll-run-svc/internal/domain"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: c}
}

// Caller is forwarded so the registry resolves the verified tenant and audits the real caller.
type Caller struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

type Policy struct {
	ID               string `json:"retention_policy_id"`
	MinRetentionDays int    `json:"min_retention_days"`
	MaxRetentionDays *int   `json:"max_retention_days,omitempty"`
}

type Hold struct {
	ID        string `json:"legal_hold_id"`
	Authority string `json:"authority"`
}

// Resolution is the registry's two independent findings: a legal hold, and the applicable policy.
type Resolution struct {
	Blocked bool    `json:"blocked"`
	Hold    *Hold   `json:"matched_hold,omitempty"`
	Policy  *Policy `json:"applicable_policy,omitempty"`
}

func (c *Client) Resolve(ctx context.Context, caller Caller, recordClass, jurisdictionCode, entityRef string) (*Resolution, error) {
	q := url.Values{"record_class": {recordClass}}
	if jurisdictionCode != "" {
		q.Set("jurisdiction_code", jurisdictionCode)
	}
	if entityRef != "" {
		q.Set("entity_ref", entityRef)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/retention/resolve?"+q.Encode(), nil)
	if err != nil {
		return nil, domain.ErrRetentionUnavailable
	}
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	req.Header.Set("X-Correlation-ID", caller.CorrelationID)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, domain.ErrRetentionUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrRetentionUnavailable
	}
	var out Resolution
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrRetentionUnavailable
	}
	if out.Blocked && out.Hold == nil {
		return nil, domain.ErrRetentionUnavailable // a block with no hold is not an answer to trust or to ignore
	}
	return &out, nil
}
