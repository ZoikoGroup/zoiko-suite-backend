// Package jurisdiction provides a client for validating jurisdiction_id
// against jurisdiction-rules-svc — critical constraint (03-microservices.md
// §8.5): every obligation must be jurisdiction-bound, so a bad or unknown
// jurisdiction_id must never be silently accepted.
//
// Mirrors tenant-entity-registry-svc's HTTPJurisdictionValidator exactly
// (internal/jurisdiction/validator.go there): fail-closed on any error —
// an unreachable jurisdiction-rules-svc must reject the obligation write,
// not silently accept an unvalidated jurisdiction_id.
package jurisdiction

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

// Validator validates that a jurisdiction_id exists before it is persisted
// on an obligation.
type Validator interface {
	// ValidateExists returns nil if jurisdictionID is known.
	// Returns domain.ErrJurisdictionNotFound if it does not exist.
	// Returns domain.ErrJurisdictionServiceUnavailable if jurisdiction-rules-svc
	// cannot be reached — callers must fail-closed.
	ValidateExists(ctx context.Context, jurisdictionID string) error
}

// HTTPValidator implements Validator against a real jurisdiction-rules-svc instance.
//
// Called synchronously on obligation creation — an obligation with an
// unvalidated jurisdiction_id would silently propagate compliance/filing
// failures across the platform, so this call is on the write path, not
// best-effort.
type HTTPValidator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// NewHTTPValidator constructs an HTTPValidator bound to baseURL, e.g.
// "http://jurisdiction-svc:8082" (no trailing slash).
func NewHTTPValidator(baseURL string, log *zap.Logger) *HTTPValidator {
	return &HTTPValidator{
		baseURL: baseURL,
		log:     log,
		client: &http.Client{
			// Tight timeout — jurisdiction validation must not stall obligation
			// writes. If jurisdiction-rules-svc is slow, the write is rejected
			// rather than hanging (fail-closed, not fail-slow).
			Timeout: 2 * time.Second,
		},
	}
}

// ValidateExists calls GET {baseURL}/v1/jurisdictions/{jurisdictionID}.
func (v *HTTPValidator) ValidateExists(ctx context.Context, jurisdictionID string) error {
	url := fmt.Sprintf("%s/v1/jurisdictions/%s", v.baseURL, jurisdictionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return domain.ErrJurisdictionServiceUnavailable
	}

	resp, err := v.client.Do(req)
	if err != nil {
		v.log.Error("jurisdiction-rules-svc unreachable — failing closed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return domain.ErrJurisdictionServiceUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return domain.ErrJurisdictionNotFound
	default:
		v.log.Error("unexpected response from jurisdiction-rules-svc — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("jurisdiction_id", jurisdictionID),
		)
		return domain.ErrJurisdictionServiceUnavailable
	}
}
