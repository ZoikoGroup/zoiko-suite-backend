// Package residency validates a document's declared residency region against
// tenant-entity-registry-svc's real GTRM tenant-region resolution
// (GET /v1/tenants/{tenantID}/residency-region), per docs/architecture/
// 01-backend.md §8.3 "jurisdiction-aware residency controls where required."
//
// Only exercised when a document declares a residency_region_code — most
// documents (PUBLIC/INTERNAL, no declared region) skip this entirely. Fails
// closed: an unreachable registry, or a resolved region that doesn't match
// the declared one, rejects the write rather than silently allowing it.
package residency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

var (
	// ErrMismatch is returned when the tenant's actual resolved region
	// differs from the document's declared residency_region_code.
	ErrMismatch = errors.New("residency: declared region does not match the tenant's resolved region")
	// ErrServiceUnavailable is returned when tenant-entity-registry-svc
	// cannot be reached or errors — callers must fail closed.
	ErrServiceUnavailable = errors.New("residency: tenant-entity-registry-svc unavailable")
)

// Validator is the narrow interface the store/handler layer depends on.
type Validator interface {
	// CheckRegion returns nil if declaredRegionCode matches tenantID's
	// actual resolved residency region. Returns ErrMismatch on a real
	// mismatch, ErrServiceUnavailable if the check could not be performed.
	CheckRegion(ctx context.Context, tenantID, declaredRegionCode string) error
}

type resolveResponse struct {
	TenantID   string `json:"tenant_id"`
	RegionCode string `json:"region_code"`
	RegionName string `json:"region_name"`
}

// HTTPValidator implements Validator against a real tenant-entity-registry-svc.
type HTTPValidator struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPValidator(baseURL string, log *zap.Logger) *HTTPValidator {
	return &HTTPValidator{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

func (v *HTTPValidator) CheckRegion(ctx context.Context, tenantID, declaredRegionCode string) error {
	url := fmt.Sprintf("%s/v1/tenants/%s/residency-region", v.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ErrServiceUnavailable
	}

	resp, err := v.http.Do(req)
	if err != nil {
		v.log.Error("residency check: tenant-entity-registry-svc unreachable — failing closed", zap.Error(err))
		return ErrServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		v.log.Error("residency check: unexpected response — failing closed", zap.Int("status", resp.StatusCode))
		return ErrServiceUnavailable
	}

	var out resolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ErrServiceUnavailable
	}
	if out.RegionCode != declaredRegionCode {
		return ErrMismatch
	}
	return nil
}

var _ Validator = (*HTTPValidator)(nil)
