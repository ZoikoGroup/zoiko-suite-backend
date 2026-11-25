// Package boardresolutions verifies, against the real board-resolutions-svc,
// that a resolution_id a caller attaches to a corporate action genuinely
// exists in the caller's tenant, belongs to the same legal entity, and has
// been PASSED by the board.
//
// Before this check, corporate-actions-svc stored whatever resolution_id the
// caller sent — including ids that did not exist anywhere — so a corporate
// action could claim board authority it never had (flagged by the 2026-07-23
// platform audit and confirmed by the 2026-10-27 live e2e run).
//
// Fail-closed throughout: a network error, timeout, or unexpected response is
// ErrServiceUnavailable and the write is refused, never waved through.
package boardresolutions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. Every one of them means the write must be refused.
var (
	// ErrResolutionNotFound: board-resolutions-svc has no such resolution in
	// the caller's tenant (it answers 404 for other tenants' ids too).
	ErrResolutionNotFound = errors.New("resolution_id does not exist in board-resolutions-svc")
	// ErrResolutionEntityMismatch: the resolution belongs to a different
	// legal entity than the corporate action.
	ErrResolutionEntityMismatch = errors.New("resolution belongs to a different legal entity")
	// ErrResolutionNotPassed: the resolution exists but the board has not
	// PASSED it (PROPOSED, OPEN, FAILED or SUPERSEDED).
	ErrResolutionNotPassed = errors.New("resolution has not been PASSED by the board")
	// ErrServiceUnavailable: board-resolutions-svc could not answer.
	ErrServiceUnavailable = errors.New("board-resolutions-svc unavailable")
)

// statusPassed is board-resolutions-svc's ResolutionStatusPassed.
const statusPassed = "PASSED"

// Caller is the identity of the request the lookup is made for.
// board-resolutions-svc requires X-Tenant-Id and X-Principal-Id on reads.
type Caller struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
}

// resolution is the subset of board-resolutions-svc's BoardResolution that
// this check needs.
type resolution struct {
	ResolutionID  string `json:"resolution_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

// Client calls board-resolutions-svc.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient builds a client for baseURL, e.g. "http://board-resolutions-svc:8122".
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// VerifyPassedResolution calls GET /v1/resolutions/{resolutionID} and returns
// nil only when the resolution exists in the caller's tenant, belongs to
// legalEntityID, and is PASSED. Any other outcome is one of the sentinel
// errors above.
func (c *Client) VerifyPassedResolution(ctx context.Context, caller Caller, resolutionID, legalEntityID string) error {
	requestURL := fmt.Sprintf("%s/v1/resolutions/%s", c.baseURL, url.PathEscape(resolutionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	setCallerEnvelope(req, caller)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// handled below
	case http.StatusNotFound:
		return ErrResolutionNotFound
	default:
		return fmt.Errorf("%w: board-resolutions-svc returned %d", ErrServiceUnavailable, resp.StatusCode)
	}

	var found resolution
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		return fmt.Errorf("%w: undecodable response: %v", ErrServiceUnavailable, err)
	}
	// A 200 with no id, or with another tenant's record, is treated as "not
	// found": the tenant scope is enforced upstream, this is defence in depth.
	if found.ResolutionID == "" || found.TenantID != caller.TenantID {
		return ErrResolutionNotFound
	}
	if found.LegalEntityID != legalEntityID {
		return ErrResolutionEntityMismatch
	}
	if found.Status != statusPassed {
		return fmt.Errorf("%w (status %s)", ErrResolutionNotPassed, found.Status)
	}
	return nil
}

// setCallerEnvelope writes the platform identity headers for this
// service-to-service read.
func setCallerEnvelope(req *http.Request, caller Caller) {
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	if caller.LegalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", caller.LegalEntityID)
	}
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", caller.CorrelationID)
	}
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
}
