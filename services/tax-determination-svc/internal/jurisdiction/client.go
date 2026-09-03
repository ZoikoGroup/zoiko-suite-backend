// Package jurisdiction validates the jurisdiction references TAX-03 requires
// against jurisdiction-rules-svc.
//
// config.JurisdictionRulesURL has been configured since this service was built
// and nothing read it, so ship-from, ship-to and the place of supply were
// whatever string the caller sent. A determination whose governing jurisdiction
// nobody recognises cannot be defended at audit, and a typo in it silently
// produces a valid-looking tax position against a jurisdiction that does not
// exist — the same class of gap as account_code with no Chart of Accounts,
// except that jurisdiction-rules-svc DOES exist to answer it.
//
// jurisdiction-rules-svc's own README names GET /v1/jurisdictions/{id} as "the
// synchronous fail-closed validation probe other services make before
// persisting a jurisdiction_id, so 503 means 'cannot answer', never 'no'".
// This is that probe.
package jurisdiction

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zoiko.io/tax-determination-svc/internal/domain"
)

// Client probes jurisdiction-rules-svc.
type Client struct {
	baseURL string
	http    *http.Client

	// Jurisdictions are platform-wide reference data that changes on the order
	// of legislation, not requests, and a determination names up to three of
	// them. Without a cache a single tax calculation makes three network round
	// trips to re-learn that "GB" is still a country.
	//
	// Positive results only. A negative is never cached: an unknown
	// jurisdiction is usually one being provisioned, and caching the "no" would
	// keep refusing determinations for a jurisdiction that has since been
	// registered.
	mu    sync.RWMutex
	known map[string]time.Time
	ttl   time.Duration
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
		known:   make(map[string]time.Time),
		ttl:     10 * time.Minute,
	}
}

// Validate reports whether jurisdictionID is one jurisdiction-rules-svc
// recognises.
//
// Returns domain.ErrJurisdictionUnknown when the service answers 404, and
// domain.ErrJurisdictionUnverifiable when it cannot be reached. The caller must
// keep those apart: the first is a caller mistake worth a 400, the second is
// "cannot answer" and must fail the determination closed rather than let it
// proceed against an unvalidated jurisdiction.
//
// An empty id passes. Ship-from and ship-to are optional under §9.J — a supply
// of services frequently has neither — and requiring them here would refuse
// determinations the contract permits.
func (c *Client) Validate(ctx context.Context, correlationID, jurisdictionID string) error {
	if jurisdictionID == "" {
		return nil
	}
	if c.cached(jurisdictionID) {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/jurisdictions/%s", c.baseURL, jurisdictionID), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrJurisdictionUnverifiable, err)
	}
	req.Header.Set("Accept", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	req.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrJurisdictionUnverifiable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		c.remember(jurisdictionID)
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", domain.ErrJurisdictionUnknown, jurisdictionID)
	default:
		return fmt.Errorf("%w: jurisdiction-rules-svc returned %d", domain.ErrJurisdictionUnverifiable, resp.StatusCode)
	}
}

func (c *Client) cached(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen, ok := c.known[id]
	return ok && time.Since(seen) < c.ttl
}

func (c *Client) remember(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known[id] = time.Now()
}
