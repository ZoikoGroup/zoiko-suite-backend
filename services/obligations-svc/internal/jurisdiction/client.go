// Package jurisdiction provides a client for validating jurisdiction_id
// against jurisdiction-rules-svc — critical constraint (03-microservices.md
// §8.5): every obligation must be jurisdiction-bound, so a bad or unknown
// jurisdiction_id must never be silently accepted.
//
// Mirrors tenant-entity-registry-svc's HTTPJurisdictionValidator exactly
// (internal/jurisdiction/validator.go there): fail-closed on any error —
// an unreachable jurisdiction-rules-svc must reject the obligation write,
// not silently accept an unvalidated jurisdiction_id.
//
// Circuit breaker added per 03-microservices.md §17.7: "All inter-service
// calls must use circuit breakers, timeouts, retries, and dead-letter routing
// where applicable. A Tier 2 failure must not collapse a Tier 0 process."
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
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/circuitbreaker"
	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// Validator validates that a jurisdiction_id exists before it is persisted
// on an obligation.
type Validator interface {
	// ValidateExists returns nil if jurisdictionID is known.
	// Returns domain.ErrJurisdictionNotFound if it does not exist.
	// Returns domain.ErrJurisdictionServiceUnavailable if jurisdiction-rules-svc
	// cannot be reached — callers must fail-closed.
	ValidateExists(ctx context.Context, jurisdictionID string) error

	// ValidateCalendarRule returns nil if the calendar rule exists and matches
	// the expected version. Returns domain.ErrJurisdictionServiceUnavailable
	// if jurisdiction-rules-svc cannot be reached — callers must fail-closed.
	ValidateCalendarRule(ctx context.Context, calendarCode, calendarRuleID string, calendarRuleVersion int) error
}

// HTTPValidator implements Validator against a real jurisdiction-rules-svc instance.
//
// Called synchronously on obligation creation — an obligation with an
// unvalidated jurisdiction_id would silently propagate compliance/filing
// failures across the platform, so this call is on the write path, not
// best-effort.
//
// Circuit breaker prevents cascading failures if jurisdiction-rules-svc is down.
type HTTPValidator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
	breaker *circuitbreaker.CircuitBreaker
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
		breaker: circuitbreaker.NewCircuitBreaker("jurisdiction-rules-svc", circuitbreaker.DefaultConfig()),
	}
}

// ValidateExists calls GET {baseURL}/v1/jurisdictions/{jurisdictionID}.
func (v *HTTPValidator) ValidateExists(ctx context.Context, jurisdictionID string) error {
	err := v.breaker.Execute(ctx, func() error {
		// The id comes from the request body; escape it so it can only ever
		// name one path segment.
		target := fmt.Sprintf("%s/v1/jurisdictions/%s", v.baseURL, url.PathEscape(jurisdictionID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return domain.ErrJurisdictionServiceUnavailable
		}
		svcenvelope.ForwardTo(ctx, req)
		idenv.Forward(ctx, req)

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
	})

	if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		v.log.Error("circuit breaker open for jurisdiction-rules-svc — failing closed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.String("breaker_state", v.breaker.GetState().String()),
		)
		return domain.ErrJurisdictionServiceUnavailable
	}

	return err
}

// ValidateCalendarRule calls GET {baseURL}/v1/calendars/{calendarCode}/rules/{calendarRuleID}
// to verify the calendar rule exists and matches the expected version.
func (v *HTTPValidator) ValidateCalendarRule(ctx context.Context, calendarCode, calendarRuleID string, calendarRuleVersion int) error {
	err := v.breaker.Execute(ctx, func() error {
		target := fmt.Sprintf("%s/v1/calendars/%s/rules/%s", v.baseURL, url.PathEscape(calendarCode), url.PathEscape(calendarRuleID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return domain.ErrJurisdictionServiceUnavailable
		}
		svcenvelope.ForwardTo(ctx, req)
		idenv.Forward(ctx, req)

		resp, err := v.client.Do(req)
		if err != nil {
			v.log.Error("jurisdiction-rules-svc unreachable for calendar rule — failing closed",
				zap.String("calendar_code", calendarCode),
				zap.String("calendar_rule_id", calendarRuleID),
				zap.Error(err),
			)
			return domain.ErrJurisdictionServiceUnavailable
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			// Check the rule version matches
			var body struct {
				Version int `json:"version"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				v.log.Error("unexpected response body from jurisdiction-rules-svc calendar rule",
					zap.Error(err))
				return domain.ErrJurisdictionServiceUnavailable
			}
			if body.Version != calendarRuleVersion {
				v.log.Error("calendar rule version mismatch",
					zap.String("calendar_code", calendarCode),
					zap.String("calendar_rule_id", calendarRuleID),
					zap.Int("expected_version", calendarRuleVersion),
					zap.Int("actual_version", body.Version),
				)
				return domain.ErrJurisdictionServiceUnavailable
			}
			return nil
		case http.StatusNotFound:
			return domain.ErrJurisdictionServiceUnavailable // treat as unavailable, fail-closed
		default:
			v.log.Error("unexpected response from jurisdiction-rules-svc calendar rule — failing closed",
				zap.Int("status", resp.StatusCode),
				zap.String("calendar_code", calendarCode),
				zap.String("calendar_rule_id", calendarRuleID),
			)
			return domain.ErrJurisdictionServiceUnavailable
		}
	})

	if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		v.log.Error("circuit breaker open for jurisdiction-rules-svc calendar rule — failing closed",
			zap.String("calendar_code", calendarCode),
			zap.String("calendar_rule_id", calendarRuleID),
			zap.String("breaker_state", v.breaker.GetState().String()),
		)
		return domain.ErrJurisdictionServiceUnavailable
	}

	return err
}
