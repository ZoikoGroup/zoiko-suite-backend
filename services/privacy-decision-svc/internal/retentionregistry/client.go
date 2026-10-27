// Package retentionregistry is a real HTTP client to
// retention-registry-svc, used ONLY when the caller supplies its own
// real record_class — this service never invents one.
package retentionregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("retention-registry-svc unavailable")

type LegalHold struct {
	LegalHoldID string `json:"legal_hold_id"`
}

type RetentionResolution struct {
	Blocked     bool       `json:"blocked"`
	MatchedHold *LegalHold `json:"matched_hold,omitempty"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// Resolve calls GET /v1/retention/resolve?record_class=&entity_ref=.
// Forwards verified tenant and principal identity.
func (c *Client) Resolve(ctx context.Context, tenantID, principalID, recordClass, entityRef string) (*RetentionResolution, error) {
	u := c.baseURL + "/v1/retention/resolve?record_class=" + url.QueryEscape(recordClass)
	if entityRef != "" {
		u += "&entity_ref=" + url.QueryEscape(entityRef)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	var res RetentionResolution
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, ErrUnavailable
	}
	return &res, nil
}
