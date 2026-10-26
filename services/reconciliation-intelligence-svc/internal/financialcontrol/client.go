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
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
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
	DateToleranceDays *int   `json:"date_tolerance_days"`
	DateBasis         string `json:"date_basis"`
}

// ErrUnsupportedDateBasis is a governed date tolerance counted in BUSINESS
// days. No business-day calendar exists on the platform (financial-control-
// svc's own matcher counts calendar days too), so it is refused rather than
// silently counted as calendar days.
var ErrUnsupportedDateBasis = errors.New("governed date tolerance uses date_basis BUSINESS, which has no business-day calendar to count against")

type listTolerancePoliciesResponse struct {
	Items []tolerancePolicy `json:"items"`
}

// GetActiveAbsoluteTolerance fetches the current (highest-version)
// governed absolute tolerance for (legalEntityID, metric), returning both the
// value and the version it was approved under. Fails closed: unreachable,
// non-200, or a malformed body all return ErrServiceUnavailable; a reachable
// service with no policy configured yet returns ErrToleranceNotConfigured.
// Neither case ever falls back to a guessed default — see the package doc
// comment.
//
// financial-control-svc returns the tolerance as an exact decimal string;
// it is parsed straight into a big.Rat, never float64 (ZS-DATA-001 D06),
// so the governed limit a suggestion is compared against is the exact
// value that was approved. A negative tolerance or a non-positive version
// is malformed and also fails closed.
//
// The version used to be discarded. It is now returned so every suggestion
// can record which policy version its score basis rests on (A8).
func (c *Client) GetActiveAbsoluteTolerance(ctx context.Context, tenantID, principalID, legalEntityID, metric string) (*domain.GovernedTolerance, error) {
	latest, err := c.latestPolicy(ctx, tenantID, principalID, legalEntityID, metric)
	if err != nil {
		return nil, err
	}
	tolerance, err := domain.ParseAmount(latest.AbsoluteTolerance)
	if err != nil || tolerance.Sign() < 0 {
		return nil, fmt.Errorf("%w: malformed absolute_tolerance %q", ErrServiceUnavailable, latest.AbsoluteTolerance)
	}
	return &domain.GovernedTolerance{Amount: tolerance, Version: latest.ToleranceVersion}, nil
}

// GetActiveDateTolerance fetches the current governed date tolerance for
// (legalEntityID, metric): the policy row's date_tolerance_days and version.
// The same fail-closed rules as GetActiveAbsoluteTolerance apply — no policy
// is ErrToleranceNotConfigured, never a default — and a BUSINESS basis is
// ErrUnsupportedDateBasis.
func (c *Client) GetActiveDateTolerance(ctx context.Context, tenantID, principalID, legalEntityID, metric string) (*domain.DateTolerance, error) {
	latest, err := c.latestPolicy(ctx, tenantID, principalID, legalEntityID, metric)
	if err != nil {
		return nil, err
	}
	if latest.DateToleranceDays == nil || *latest.DateToleranceDays < 0 {
		return nil, fmt.Errorf("%w: malformed date_tolerance_days", ErrServiceUnavailable)
	}
	switch latest.DateBasis {
	case "CALENDAR":
	case "BUSINESS":
		return nil, ErrUnsupportedDateBasis
	default:
		return nil, fmt.Errorf("%w: malformed date_basis %q", ErrServiceUnavailable, latest.DateBasis)
	}
	return &domain.DateTolerance{Days: *latest.DateToleranceDays, Version: latest.ToleranceVersion}, nil
}

// latestPolicy reads the current (highest-version) policy row for
// (legalEntityID, metric) and checks its version.
func (c *Client) latestPolicy(ctx context.Context, tenantID, principalID, legalEntityID, metric string) (*tolerancePolicy, error) {
	q := url.Values{"legal_entity_id": {legalEntityID}, "metric": {metric}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/controls/v1/tolerance-policies?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	setEnvelopeHeaders(ctx, req, tenantID, principalID, legalEntityID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			c.logger.Error("financial-control-svc unreachable — failing closed", zap.Error(err))
		}
		return nil, ErrServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrServiceUnavailable
	}

	var out listTolerancePoliciesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrServiceUnavailable
	}
	if len(out.Items) == 0 {
		return nil, ErrToleranceNotConfigured
	}

	// ListTolerancePolicies orders by (metric, tolerance_version)
	// ascending, and CreateTolerancePolicy never edits a prior version
	// in place — the last item is the current policy.
	latest := out.Items[len(out.Items)-1]
	if latest.ToleranceVersion <= 0 {
		return nil, fmt.Errorf("%w: malformed tolerance_version %d", ErrServiceUnavailable, latest.ToleranceVersion)
	}
	return &latest, nil
}

// setEnvelopeHeaders forwards the canonical service input envelope
// (ZS-ARCH-SVC-001 §4). financial-control-svc requires tenant, actor,
// request id, correlation id and source channel on every request, reads
// included. Same pattern as internal/bankrec: the caller's values from the
// envelope in context, an id minted only when none was supplied.
func setEnvelopeHeaders(ctx context.Context, req *http.Request, tenantID, principalID, legalEntityID string) {
	req.Header.Set(svcenvelope.HeaderTenantID, tenantID)
	req.Header.Set(svcenvelope.HeaderActorSubjectID, principalID)
	if legalEntityID != "" {
		req.Header.Set(svcenvelope.HeaderLegalEntityID, legalEntityID)
	}

	requestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
	sourceChannel := string(svcenvelope.ChannelSystem)
	correlationID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		// An external channel (import, integration) obliges source_system;
		// forwarding the channel without it would violate the contract.
		if env.SourceSystem != "" {
			req.Header.Set(svcenvelope.HeaderSourceSystem, env.SourceSystem)
		}
		if env.CorrelationID != "" {
			correlationID = env.CorrelationID
		}
		if env.CausationID != "" {
			req.Header.Set(svcenvelope.HeaderCausationID, env.CausationID)
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set(svcenvelope.HeaderRequestID, requestID)
	req.Header.Set(svcenvelope.HeaderCorrelationID, correlationID)
	req.Header.Set(svcenvelope.HeaderSourceChannel, sourceChannel)
}
