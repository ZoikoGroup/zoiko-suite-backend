// Package retentionregistry is a real HTTP client to
// retention-registry-svc, used ONLY when the caller supplies its own
// real record_class — this service never invents one. See the domain
// package's doc comment for why: evidence-manifest-svc's own
// legal-hold finding (master-register-findings-2026-08-27.md §2.3) was
// re-scoped rather than built for exactly this reason — a made-up
// record_class would silently miss any hold scoped to the real one,
// which is worse than not checking at all.
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
// tenant_id is deliberately NOT passed as a query parameter — the real
// resolve route in retention-registry-svc takes tenant from the verified
// X-Tenant-Id header, never a caller-supplied query value (see that
// service's own resolveTenantScope doc comment) — so this client forwards
// the header rather than trying to pass tenant another way.
func (c *Client) Resolve(ctx context.Context, tenantID, recordClass, entityRef string) (*RetentionResolution, error) {
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
