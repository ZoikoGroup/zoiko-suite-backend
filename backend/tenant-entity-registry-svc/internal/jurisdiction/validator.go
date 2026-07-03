// Package jurisdiction provides the jurisdiction validator interface and client.
//
// Q2 resolution: jurisdiction_id references on EntityJurisdictionAssignment
// must be synchronously validated against the Jurisdiction Rules Service
// before persistence. If the Jurisdiction Rules Service is unreachable,
// the assignment is REJECTED (fail-closed). A bad jurisdiction reference
// would silently propagate tax, payroll, and filing failures across the platform.
//
// The JurisdictionValidator interface is a clean abstraction so this service
// can be built and tested now — the real HTTP client is a drop-in swap once
// the Jurisdiction Rules Service ships.
package jurisdiction

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// JurisdictionValidator validates that a jurisdiction_id exists in the
// Jurisdiction Rules Service before it is persisted on an assignment.
type JurisdictionValidator interface {
	// ValidateExists returns nil if the jurisdiction_id is known and active.
	// Returns ErrJurisdictionNotFound if the ID does not exist.
	// Returns ErrValidatorUnavailable if the service cannot be reached — callers fail-closed.
	ValidateExists(ctx context.Context, jurisdictionID string) error
}

var (
	// ErrJurisdictionNotFound is returned when the jurisdiction_id does not exist.
	ErrJurisdictionNotFound = fmt.Errorf("jurisdiction not found")
	// ErrValidatorUnavailable is returned when the Jurisdiction Rules Service is unreachable.
	// All assignment attempts must be rejected when this error is returned (fail-closed).
	ErrValidatorUnavailable = fmt.Errorf("jurisdiction rules service unavailable")
)

// StubJurisdictionValidator is the development stub.
// Accepts any jurisdiction_id. Replace with HTTPJurisdictionValidator
// once the Jurisdiction Rules Service ships.
type StubJurisdictionValidator struct {
	log *zap.Logger
}

func NewStubValidator(log *zap.Logger) *StubJurisdictionValidator {
	return &StubJurisdictionValidator{log: log}
}

func (v *StubJurisdictionValidator) ValidateExists(ctx context.Context, jurisdictionID string) error {
	v.log.Debug("jurisdiction validator stub — accepted (wire real service when available)",
		zap.String("jurisdiction_id", jurisdictionID),
	)
	return nil
}

// HTTPJurisdictionValidator is the production implementation.
// Called synchronously on jurisdiction assignment creation (Q2 — fail-closed).
type HTTPJurisdictionValidator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPValidator(baseURL string, log *zap.Logger) *HTTPJurisdictionValidator {
	return &HTTPJurisdictionValidator{
		baseURL: baseURL,
		log:     log,
		client: &http.Client{
			// Tight timeout — jurisdiction validation must not stall entity writes.
			// If the Jurisdiction Rules Service is slow, assignments are rejected.
			Timeout: 2 * time.Second,
		},
	}
}

func (v *HTTPJurisdictionValidator) ValidateExists(ctx context.Context, jurisdictionID string) error {
	// TODO: GET {baseURL}/v1/jurisdictions/{jurisdictionID}
	// 200 → valid; 404 → ErrJurisdictionNotFound; network err → ErrValidatorUnavailable
	url := fmt.Sprintf("%s/v1/jurisdictions/%s", v.baseURL, jurisdictionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ErrValidatorUnavailable
	}
	resp, err := v.client.Do(req)
	if err != nil {
		v.log.Error("jurisdiction rules service unreachable — failing closed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return ErrValidatorUnavailable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrJurisdictionNotFound, jurisdictionID)
	default:
		v.log.Error("unexpected response from jurisdiction rules service — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("jurisdiction_id", jurisdictionID),
		)
		return ErrValidatorUnavailable
	}
}
