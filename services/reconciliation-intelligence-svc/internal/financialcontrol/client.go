// Package financialcontrol is a thin client for financial-control-svc's
// governed TolerancePolicy registry (ZS-CONTROL-001).
//
// This exists to close a named, self-documented gap: reconciliation
// heuristics in internal/domain previously used hardcoded, unversioned
// Go constants for monetary write-off tolerances — ZS-SVC-Z-001 INV-09
// requires tolerance be "explicit and versioned." financial-control-svc
// already has exactly that machinery (TolerancePolicy: versioned,
// maker-checker approved, append-only). This client fetches the current
// policy instead of the service inventing its own.
package financialcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// ErrServiceUnavailable is returned for any transport error, non-200
// response, or undecodable body. Callers must treat this as a hard
// refusal to proceed, never as "assume no tolerance configured" —
// those are different failure modes (see ErrToleranceNotConfigured).
var ErrServiceUnavailable = errors.New("financial-control-svc unavailable")

// ErrToleranceNotConfigured means financial-control-svc answered
// successfully but no TolerancePolicy exists yet for this
// (legal_entity_id, metric). This is not a transport failure — it means
// nobody has governed this tolerance yet, which is itself a reason to
// refuse rather than guess.
var ErrToleranceNotConfigured = errors.New("no governed tolerance policy configured for this legal entity and metric")

type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(baseURL string, logger *zap.Logger) *Client {
	return &Client{baseURL: baseURL, httpClient: &http.Client{Timeout: 5 * time.Second}, logger: logger}
}

type tolerancePolicy struct {
	ToleranceVersion  int    `json:"tolerance_version"`
	AbsoluteTolerance string `json:"absolute_tolerance"`
}

type listTolerancePoliciesResponse struct {
	Items []tolerancePolicy `json:"items"`
}

// GetActiveAbsoluteTolerance fetches the current (highest-version)
// governed absolute tolerance for (legalEntityID, metric). Fails
// closed: unreachable, non-200, or a malformed body all return
// ErrServiceUnavailable; a reachable service with no policy configured
// yet returns ErrToleranceNotConfigured. Neither case ever falls back
// to a guessed default — see the package doc comment.
func (c *Client) GetActiveAbsoluteTolerance(ctx context.Context, tenantID, principalID, legalEntityID, metric string) (float64, error) {
	q := url.Values{"legal_entity_id": {legalEntityID}, "metric": {metric}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/controls/v1/tolerance-policies?"+q.Encode(), nil)
	if err != nil {
		return 0, ErrServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			c.logger.Error("financial-control-svc unreachable — failing closed", zap.Error(err))
		}
		return 0, ErrServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, ErrServiceUnavailable
	}

	var out listTolerancePoliciesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, ErrServiceUnavailable
	}
	if len(out.Items) == 0 {
		return 0, ErrToleranceNotConfigured
	}

	// ListTolerancePolicies orders by (metric, tolerance_version)
	// ascending, and CreateTolerancePolicy never edits a prior version
	// in place — the last item is the current policy.
	latest := out.Items[len(out.Items)-1]
	tolerance, err := strconv.ParseFloat(latest.AbsoluteTolerance, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed absolute_tolerance %q", ErrServiceUnavailable, latest.AbsoluteTolerance)
	}
	return tolerance, nil
}
