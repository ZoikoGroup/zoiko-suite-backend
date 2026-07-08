// Package jurisdiction provides a client for validating jurisdiction_id
// against jurisdiction-rules-svc, used when creating a jurisdiction-scoped
// SoD rule. Mirrors obligations-svc's and tenant-entity-registry-svc's
// validator exactly: fail-closed on any error.
package jurisdiction

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// Validator validates that a jurisdiction_id exists.
type Validator interface {
	ValidateExists(ctx context.Context, jurisdictionID string) error
}

// HTTPValidator implements Validator against a real jurisdiction-rules-svc instance.
type HTTPValidator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPValidator(baseURL string, log *zap.Logger) *HTTPValidator {
	return &HTTPValidator{
		baseURL: baseURL,
		log:     log,
		client:  &http.Client{Timeout: 2 * time.Second},
	}
}

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
