package jurisdiction

// Deadline computation for WFC-05 clocks (R-001 s7.1, R-INV-16; tracker
// OB-S6). obligations-svc never computes a due date itself: it asks
// jurisdiction-rules-svc, which derives it from a released, signed pack (rule,
// calendar version, time zone, business-day convention, extension, cutoff,
// DST rule JR-DST-1) and records the calculation as evidence. The answer is
// stored verbatim on the clock.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/circuitbreaker"
	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// CalculationRequest is the body of POST /v1/obligation-calculations:calculate.
type CalculationRequest struct {
	Jurisdiction   string `json:"jurisdiction"`
	ObligationCode string `json:"obligation_code"`
	domain.DueFacts
	PackRef     string `json:"pack_ref,omitempty"`
	PackVersion string `json:"pack_version,omitempty"`
}

// Calculator asks jurisdiction-rules-svc for a due date.
type Calculator interface {
	// Calculate returns the calculation for any answered outcome (the caller
	// refuses anything but DUE_DATE_CALCULATED). It returns
	// domain.ErrJurisdictionServiceUnavailable when the service cannot answer
	// and domain.ErrAuthorizationDenied when the caller may not calculate.
	Calculate(ctx context.Context, req CalculationRequest, idempotencyKey string) (*domain.Calculation, error)
}

// SystemIdentity names the principal a system worker (the calendar
// consumer) calculates as when no request envelope is in the context.
type SystemIdentity struct {
	PrincipalID string
	TenantID    string
}

type systemIdentityKey struct{}

// WithSystemIdentity marks ctx as a system worker's call for one tenant.
func WithSystemIdentity(ctx context.Context, id SystemIdentity) context.Context {
	return context.WithValue(ctx, systemIdentityKey{}, id)
}

// HTTPCalculator calls jurisdiction-rules-svc.
type HTTPCalculator struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
	breaker *circuitbreaker.CircuitBreaker
}

// NewHTTPCalculator binds a calculator to baseURL (no trailing slash).
func NewHTTPCalculator(baseURL string, log *zap.Logger) *HTTPCalculator {
	return &HTTPCalculator{baseURL: baseURL, log: log, client: &http.Client{Timeout: 5 * time.Second},
		breaker: circuitbreaker.NewCircuitBreaker("jurisdiction-rules-svc-calculate", circuitbreaker.DefaultConfig())}
}

type dueDecisionWire struct {
	Outcome string `json:"outcome"`
	Pack    *struct {
		PackRef       string `json:"pack_ref"`
		Version       string `json:"version"`
		PackVersionID string `json:"pack_version_id"`
	} `json:"pack"`
	Calculation *struct {
		Outcome          string     `json:"outcome"`
		Message          string     `json:"message"`
		ObligationRuleID string     `json:"obligation_rule_id"`
		RuleVersion      int        `json:"rule_version"`
		ContentDigest    string     `json:"content_digest"`
		AnchorDate       string     `json:"anchor_date"`
		UnadjustedDate   string     `json:"unadjusted_date"`
		DueDate          string     `json:"due_date"`
		DueAt            *time.Time `json:"due_at"`
		Timezone         string     `json:"timezone"`
		CalendarCode     string     `json:"calendar_code"`
		CalendarVersion  int        `json:"calendar_version"`
		CalendarVerID    string     `json:"calendar_version_id"`
		ExtensionGranted int        `json:"extension_days_granted"`
		BusinessDayAdj   string     `json:"business_day_adjustment"`
		Explanation      []string   `json:"explanation"`
	} `json:"calculation"`
	Explanation []string `json:"explanation"`
}

type calculateResponse struct {
	DecisionID  string          `json:"decision_id"`
	Calculation dueDecisionWire `json:"calculation"`
}

// Calculate implements Calculator.
func (c *HTTPCalculator) Calculate(ctx context.Context, in CalculationRequest, idempotencyKey string) (*domain.Calculation, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out *domain.Calculation
	err = c.breaker.Execute(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/obligation-calculations:calculate", bytes.NewReader(body))
		if err != nil {
			return domain.ErrJurisdictionServiceUnavailable
		}
		req.Header.Set("Content-Type", "application/json")
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if sys, ok := ctx.Value(systemIdentityKey{}).(SystemIdentity); ok {
			req.Header.Set("X-Principal-Id", sys.PrincipalID)
			req.Header.Set("X-Tenant-Id", sys.TenantID)
			req.Header.Set("X-Correlation-ID", "obligations-calendar-"+idempotencyKey)
		} else {
			svcenvelope.ForwardTo(ctx, req)
			idenv.Forward(ctx, req)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			c.log.Error("jurisdiction-rules-svc unreachable for a due-date calculation; failing closed", zap.Error(err))
			return domain.ErrJurisdictionServiceUnavailable
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		switch resp.StatusCode {
		case http.StatusOK, http.StatusUnprocessableEntity, http.StatusConflict:
		case http.StatusForbidden, http.StatusUnauthorized:
			return domain.ErrAuthorizationDenied
		case http.StatusBadRequest:
			return fmt.Errorf("%w: jurisdiction-rules-svc refused the facts: %s", domain.ErrInvalidField, truncate(raw))
		default:
			c.log.Error("unexpected answer from jurisdiction-rules-svc calculation; failing closed", zap.Int("status", resp.StatusCode))
			return domain.ErrJurisdictionServiceUnavailable
		}
		var cr calculateResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return domain.ErrJurisdictionServiceUnavailable
		}
		out = toCalculation(cr)
		if resp.StatusCode == http.StatusConflict && out.Outcome == "" {
			// a 409 with no calculation body is an idempotency conflict
			return fmt.Errorf("%w: jurisdiction-rules-svc: %s", domain.ErrConflict, truncate(raw))
		}
		return nil
	})
	if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		return nil, domain.ErrJurisdictionServiceUnavailable
	}
	return out, err
}

func toCalculation(cr calculateResponse) *domain.Calculation {
	d := cr.Calculation
	out := &domain.Calculation{DecisionID: cr.DecisionID, Outcome: d.Outcome}
	explain := d.Explanation
	if d.Pack != nil {
		out.PackRef, out.PackVersion, out.PackVersionID = d.Pack.PackRef, d.Pack.Version, d.Pack.PackVersionID
	}
	if c := d.Calculation; c != nil {
		if c.Outcome != "" {
			out.Outcome = c.Outcome
		}
		out.Message = c.Message
		out.ObligationRuleID, out.RuleVersion, out.ContentDigest = c.ObligationRuleID, c.RuleVersion, c.ContentDigest
		out.AnchorDate, out.UnadjustedDate, out.DueDate, out.DueAt = c.AnchorDate, c.UnadjustedDate, c.DueDate, c.DueAt
		out.Timezone, out.CalendarCode, out.CalendarVersion, out.CalendarVersionID = c.Timezone, c.CalendarCode, c.CalendarVersion, c.CalendarVerID
		out.ExtensionGranted, out.BusinessDayAdjustment = c.ExtensionGranted, c.BusinessDayAdj
		if len(c.Explanation) > 0 {
			explain = c.Explanation
		}
	}
	if explain == nil {
		explain = []string{}
	}
	out.Explanation, _ = json.Marshal(explain)
	return out
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}
