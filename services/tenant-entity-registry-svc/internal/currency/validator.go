// Package currency validates currency codes against currency-registry-svc, read-only.
//
// REF-03 and the other reference families name currencies but do not own them: currency identity belongs to
// currency-registry-svc (REF-02). A code is checked there before it is accepted, and the answer is fail-closed: a
// registry that cannot be reached means the observation is not accepted, never that the code is assumed good.
package currency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	svcenvelope "zoiko.io/tenant-entity-registry-svc/internal/envelope"
)

// Result is what the registry says about one code.
type Result struct {
	Supported bool
	Reason    string
	// MinorUnit is the number of minor-unit digits the registry reports, when it reports one.
	MinorUnit *int
}

// Validator checks a currency code. A Result with Supported false is an answer (the code is unknown or not
// supported); an error is ErrUnavailable and the caller fails closed.
type Validator interface {
	Validate(ctx context.Context, code string) (Result, error)
}

// ErrUnavailable is returned when the registry cannot answer.
var ErrUnavailable = errors.New("currency registry unavailable")

var alpha3 = regexp.MustCompile(`^[A-Z]{3}$`)

// Stub accepts any well-formed ISO 4217 alphabetic code. It exists for development only: it validates shape, not
// existence, and main.go refuses to use it outside dev and test.
type Stub struct{ log *zap.Logger }

// NewStub builds the development stub.
func NewStub(log *zap.Logger) *Stub { return &Stub{log: log} }

// Validate implements Validator.
func (s *Stub) Validate(_ context.Context, code string) (Result, error) {
	if !alpha3.MatchString(code) {
		return Result{Reason: "MALFORMED"}, nil
	}
	s.log.Debug("currency validator stub: accepted by shape only", zap.String("code", code))
	return Result{Supported: true}, nil
}

// Unavailable is the validator used when no registry is configured outside development: every check fails closed.
type Unavailable struct{}

// Validate implements Validator.
func (Unavailable) Validate(context.Context, string) (Result, error) {
	return Result{}, fmt.Errorf("%w: CURRENCY_REGISTRY_URL is not configured", ErrUnavailable)
}

// HTTP is the production validator.
type HTTP struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// NewHTTP builds a validator that calls currency-registry-svc.
func NewHTTP(baseURL string, log *zap.Logger) *HTTP {
	return &HTTP{baseURL: baseURL, log: log, client: &http.Client{Timeout: 2 * time.Second}}
}

// Validate asks the registry whether the code exists. operation=read: a rate is a reference fact about a currency,
// not a new posting, so any currency that ever existed is acceptable for the historical record.
func (h *HTTP) Validate(ctx context.Context, code string) (Result, error) {
	if !alpha3.MatchString(code) {
		return Result{Reason: "MALFORMED"}, nil
	}
	u := fmt.Sprintf("%s/v1/currencies:validate?code=%s&operation=read", h.baseURL, url.QueryEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, ErrUnavailable
	}
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)
	resp, err := h.client.Do(req)
	if err != nil {
		h.log.Error("currency registry unreachable: failing closed", zap.String("code", code), zap.Error(err))
		return Result{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.log.Error("unexpected response from currency registry: failing closed", zap.Int("status", resp.StatusCode), zap.String("code", code))
		return Result{}, ErrUnavailable
	}
	var body struct {
		Supported bool   `json:"supported"`
		Reason    string `json:"reason"`
		MinorUnit *int   `json:"minor_unit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		h.log.Error("unreadable currency registry answer: failing closed", zap.String("code", code), zap.Error(err))
		return Result{}, ErrUnavailable
	}
	return Result{Supported: body.Supported, Reason: body.Reason, MinorUnit: body.MinorUnit}, nil
}
