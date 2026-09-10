// Package killswitch is a real client of kill-switch-registry-svc's
// resolve endpoint — the incident-response registry this service's own
// automation_policies.kill_switch_engaged column was standing in for.
//
// That column is a static bool, set once when a policy is created and
// changeable only by re-issuing the whole policy row. kill-switch-registry-svc
// exists precisely so an operator can halt AI automation for a domain/
// provider/tenant scope during an incident without touching any policy
// data at all, and so the halt is a governed, approved, logged event
// rather than an ad hoc row edit. This client layers that live check on
// top of the existing static-bool resolution — see handler.resolveBlocked.
package killswitch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrServiceUnavailable is returned when kill-switch-registry-svc cannot be
// reached or returns a non-2xx response. The caller decides fail-open vs.
// fail-closed; for an automation-authorization gate that decision is
// fail-closed (see handler.resolveBlocked's doc comment).
var ErrServiceUnavailable = errors.New("kill-switch-registry-svc unavailable")

type Checker interface {
	// Resolve reports whether AI automation for this (domain, providerCode,
	// tenantID) scope is currently blocked by an engaged kill switch.
	Resolve(ctx context.Context, domainName, providerCode, tenantID string) (blocked bool, err error)
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *Client) Resolve(ctx context.Context, domainName, providerCode, tenantID string) (bool, error) {
	q := url.Values{}
	q.Set("domain", domainName)
	if providerCode != "" {
		q.Set("provider_code", providerCode)
	}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/kill-switches/resolve?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, ErrServiceUnavailable
	}

	var res struct {
		Blocked bool `json:"blocked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false, ErrServiceUnavailable
	}
	return res.Blocked, nil
}

var _ Checker = (*Client)(nil)
