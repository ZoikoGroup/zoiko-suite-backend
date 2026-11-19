// Package jurisdiction validates a market_code against jurisdiction-rules-svc
// before it is persisted on a market_releases row — Gap 3 fix (2026-11-19
// audit): market_code was previously only checked for non-emptiness, never
// against any real jurisdiction/market taxonomy, even though doc7 §Q1
// requires jurisdiction/entity/language gating.
//
// Mirrors the established pattern other services already use for the same
// call (obligations-svc's internal/jurisdiction, tenant-entity-registry-svc's
// internal/jurisdiction/validator.go): fail-closed on any error — an
// unreachable jurisdiction-rules-svc rejects the write, it never silently
// accepts an unvalidated market_code.
package jurisdiction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// Validator checks that a market_code names a real, known jurisdiction
// before it is persisted on a market_releases row.
type Validator interface {
	// ValidateExists returns nil if marketCode is a known jurisdiction code.
	// Returns ErrMarketCodeNotFound if it is not. Returns
	// ErrServiceUnavailable if jurisdiction-rules-svc cannot be reached or
	// answers unexpectedly — callers must fail-closed on this.
	ValidateExists(ctx context.Context, marketCode string) error
}

var (
	// ErrMarketCodeNotFound: jurisdiction-rules-svc has no jurisdiction
	// known by this code.
	ErrMarketCodeNotFound = errors.New("market_code is not a known jurisdiction code")
	// ErrServiceUnavailable: jurisdiction-rules-svc could not be reached or
	// returned something other than 200/404/410. Every caller must treat
	// this as a rejection, never as an implicit pass.
	ErrServiceUnavailable = errors.New("jurisdiction-rules-svc unavailable")
)

// HTTPValidator is the production implementation, calling jurisdiction-rules-svc's
// own code-resolution endpoint.
type HTTPValidator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// NewHTTPValidator constructs an HTTPValidator bound to baseURL, e.g.
// "http://jurisdiction-rules-svc:8xxx" (no trailing slash).
func NewHTTPValidator(baseURL string, log *zap.Logger) *HTTPValidator {
	return &HTTPValidator{
		baseURL: baseURL,
		log:     log,
		client: &http.Client{
			// Tight timeout — jurisdiction validation must not stall a
			// market-release write. A slow jurisdiction-rules-svc means the
			// write is rejected, not hung (fail-closed, not fail-slow).
			Timeout: 2 * time.Second,
		},
	}
}

// ValidateExists calls GET {baseURL}/v1/jurisdiction-resolutions?code={marketCode}.
// With no "system" query parameter, jurisdiction-rules-svc resolves the code
// as a jurisdiction's own internal code — exactly what market_code names
// here (a market/jurisdiction identifier this platform's own registries
// use, e.g. "GB", "DE", "US" — not a third-party system's code, which would
// need the code-mapping endpoint and a "system" instead).
func (v *HTTPValidator) ValidateExists(ctx context.Context, marketCode string) error {
	target := fmt.Sprintf("%s/v1/jurisdiction-resolutions?code=%s", v.baseURL, url.QueryEscape(marketCode))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ErrServiceUnavailable
	}

	resp, err := v.client.Do(req)
	if err != nil {
		v.log.Error("jurisdiction-rules-svc unreachable — failing closed",
			zap.String("market_code", marketCode), zap.Error(err))
		return ErrServiceUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Decode far enough to confirm the body is a real resolution, not
		// just a 200 with an empty/garbage body — defense in depth against
		// a misbehaving or mocked upstream silently passing every code.
		var out struct {
			JurisdictionID string `json:"jurisdiction_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.JurisdictionID == "" {
			v.log.Error("unreadable jurisdiction-resolutions response — failing closed",
				zap.String("market_code", marketCode), zap.Error(err))
			return ErrServiceUnavailable
		}
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrMarketCodeNotFound, marketCode)
	case http.StatusGone:
		// The code resolves, but to a retired/superseded jurisdiction — not
		// a valid market to release a capability into.
		return fmt.Errorf("%w: %s (retired jurisdiction)", ErrMarketCodeNotFound, marketCode)
	default:
		v.log.Error("unexpected response from jurisdiction-rules-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.String("market_code", marketCode))
		return ErrServiceUnavailable
	}
}
