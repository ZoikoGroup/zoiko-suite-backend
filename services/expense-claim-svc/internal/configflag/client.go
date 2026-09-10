// Package configflag calls the real configuration-feature-flag-svc for
// this service's RECEIPT_REQUIRED_THRESHOLD, replacing what used to be a
// single hardcoded env-var float (config.ReceiptRequiredThreshold)
// applied identically to every tenant on this platform.
//
// configuration-feature-flag-svc's own ConfigEntry is exactly this
// shape — a key, scoped to an environment and optionally a tenant, with a
// nil tenant meaning "the global default" — so a per-tenant receipt
// threshold is a real, structural fit for it, not an invented
// integration (see master-register-findings-2026-08-27.md §3.26).
//
// GetConfigEntry deliberately does not fall back from a tenant-specific
// miss to the global default (its own context.md §7.2) — a 404 for one
// exact (key, environment, tenant_id) tuple means only that tuple has no
// row. ResolveReceiptThreshold performs the fallback itself: try the
// tenant-scoped row, then the global row. Whether the registry has
// neither, or cannot be reached at all, is left to the caller to decide
// what to do — this is application POLICY data, not a security gate, so
// unlike kill-switch-registry-svc's fail-closed integration this one has
// no reason to block a claim approval over a config lookup; the caller
// falls back to the existing static default instead (see handler.go).
package configflag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrServiceUnavailable = errors.New("configuration-feature-flag-svc unavailable")

const receiptThresholdKey = "RECEIPT_REQUIRED_THRESHOLD"

type Client interface {
	// ResolveReceiptThreshold looks up RECEIPT_REQUIRED_THRESHOLD for
	// tenantID in environment, falling back to the global (no-tenant) row.
	// found=false with err=nil means the registry has no override at
	// either scope — a legitimate, non-error outcome. found=false with a
	// non-nil err means the registry could not be consulted at all.
	ResolveReceiptThreshold(ctx context.Context, environment, tenantID string) (threshold float64, found bool, err error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) ResolveReceiptThreshold(ctx context.Context, environment, tenantID string) (float64, bool, error) {
	if threshold, found, err := c.get(ctx, environment, tenantID, true); found || err != nil {
		return threshold, found, err
	}
	// No tenant-scoped override — try the global default (nil tenant_id
	// in the query). The caller's own tenant is still the verified
	// X-Tenant-Id on this request; only the query scope changes.
	return c.get(ctx, environment, tenantID, false)
}

// get performs one lookup. tenantID is always the caller's own verified
// tenant — it goes on X-Tenant-Id regardless of scope, since the far
// side's requireTenant refuses any request with no verified tenant at
// all, even a request asking about the global default. scoped controls
// only whether the query itself asks about tenantID's row (true) or the
// global row (false, omits ?tenant_id=).
func (c *HTTPClient) get(ctx context.Context, environment, tenantID string, scoped bool) (float64, bool, error) {
	q := url.Values{}
	q.Set("environment", environment)
	if scoped {
		q.Set("tenant_id", tenantID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/config/"+receiptThresholdKey+"?"+q.Encode(), nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, false, ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, ErrServiceUnavailable
	}

	var entry struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return 0, false, ErrServiceUnavailable
	}
	var threshold float64
	if err := json.Unmarshal(entry.Value, &threshold); err != nil {
		return 0, false, ErrServiceUnavailable
	}
	return threshold, true, nil
}

var _ Client = (*HTTPClient)(nil)
