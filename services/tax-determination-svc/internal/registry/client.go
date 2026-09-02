// Package registry resolves TAX-03's server-resolved "Registrations" input
// from tenant-entity-registry-svc.
//
// §9.J lists registrations first among the facts this service must resolve
// rather than accept: whether the seller holds a tax registration in the place
// of supply decides whether tax is charged at all, and it is not the caller's
// to assert. tenant-entity-registry-svc owns that fact as a tax identity bundle
// — one per (legal entity, jurisdiction), effective-dated and status-bearing —
// and exposes it at GET /v1/entities/{id}/tax-identity-bundles.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zoiko.io/tax-determination-svc/internal/domain"
)

// Registration is the seller's tax registration in one jurisdiction.
type Registration struct {
	BundleID       string
	JurisdictionID string
	Status         string
}

// Client reads tax identity bundles from tenant-entity-registry-svc.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. The timeout is short because this call sits in
// front of every determination: a slow registry must surface as a fast
// fail-closed refusal, not as latency on every tax calculation on the platform.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

type bundleView struct {
	TaxIdentityBundleID string  `json:"tax_identity_bundle_id"`
	LegalEntityID       string  `json:"legal_entity_id"`
	JurisdictionID      string  `json:"jurisdiction_id"`
	Status              string  `json:"status"`
	EffectiveFrom       string  `json:"effective_from"`
	EffectiveTo         *string `json:"effective_to"`
}

// ResolveSellerRegistration returns the seller entity's tax registration in
// jurisdictionID as at supplyDate, or nil when it holds none there.
//
// A nil registration is a legitimate answer, not a failure. An unregistered
// seller is a real and common state, and it is exactly the fact that decides
// whether tax is charged — so it is returned and recorded rather than treated
// as a lookup that went wrong. What IS a failure is not being able to ask:
// see domain.ErrRegistryUnavailable.
func (c *Client) ResolveSellerRegistration(
	ctx context.Context,
	tenantID, legalEntityID, jurisdictionID, supplyDate string,
) (*Registration, error) {
	if legalEntityID == "" || jurisdictionID == "" {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/entities/%s/tax-identity-bundles", c.baseURL, legalEntityID), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrRegistryUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrRegistryUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// The entity has no bundles at all. Unregistered everywhere, which is
		// an answer.
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: registry returned %d", domain.ErrRegistryUnavailable, resp.StatusCode)
	}

	// The endpoint has been seen to answer both a bare array and an object
	// wrapping one. Both are decoded rather than picking one and having the
	// other read as "no registrations" — which would silently turn a registered
	// seller into an unregistered one.
	body, err := decodeBundles(resp)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrRegistryUnavailable, err)
	}

	for _, b := range body {
		if b.JurisdictionID != jurisdictionID {
			continue
		}
		if !effectiveOn(b, supplyDate) {
			continue
		}
		return &Registration{
			BundleID:       b.TaxIdentityBundleID,
			JurisdictionID: b.JurisdictionID,
			Status:         b.Status,
		}, nil
	}
	return nil, nil
}

func decodeBundles(resp *http.Response) ([]bundleView, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	var direct []bundleView
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}

	var wrapped struct {
		Bundles            []bundleView `json:"bundles"`
		TaxIdentityBundles []bundleView `json:"tax_identity_bundles"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, errors.New("tax identity bundle response is neither an array nor a recognised wrapper")
	}
	if len(wrapped.Bundles) > 0 {
		return wrapped.Bundles, nil
	}
	return wrapped.TaxIdentityBundles, nil
}

// effectiveOn reports whether the bundle is in force on the supply date.
//
// A registration that has since been cancelled was still valid for a supply
// made while it was live, and one registered afterwards was not. Determining
// against "now" instead would silently restate historical tax positions every
// time a registration changed — the opposite of INV-09's effective-dating.
//
// An unparseable or absent supply date falls back to accepting the bundle:
// supply_date is validated before this is called, so reaching here with a bad
// one would mean refusing on a fact the caller was never asked for.
func effectiveOn(b bundleView, supplyDate string) bool {
	day, err := time.Parse("2006-01-02", supplyDate)
	if err != nil {
		return true
	}
	if from, err := parseDay(b.EffectiveFrom); err == nil && day.Before(from) {
		return false
	}
	if b.EffectiveTo != nil {
		if to, err := parseDay(*b.EffectiveTo); err == nil && day.After(to) {
			return false
		}
	}
	return true
}

// parseDay accepts the registry's RFC3339 timestamps as well as bare dates —
// TaxIdentityBundle carries time.Time fields, which marshal as RFC3339.
func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	return time.Parse("2006-01-02", s)
}
