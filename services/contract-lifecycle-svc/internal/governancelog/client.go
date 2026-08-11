// Package governancelog verifies that a contract activation is backed by a
// real, GRANTED governance decision before the contract is allowed to go
// ACTIVE — closing the gap where activation previously required only a
// signature (signed_by), with no reference to any approving decision at
// all.
package governancelog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors. Callers must fail closed: any error returned means the
// activation must be refused, same doctrine as the authz.Client in this
// service.
var (
	// ErrDecisionNotFound means governance-decision-log-svc has no such
	// decision for this tenant.
	ErrDecisionNotFound = errors.New("governance decision not found")
	// ErrDecisionNotGranted means the decision exists but was not GRANTED,
	// or does not match the expected legal_entity_id/action_type.
	ErrDecisionNotGranted = errors.New("governance decision was not granted for this action")
	// ErrServiceUnavailable means governance-decision-log-svc could not be
	// reached, timed out, or returned an unexpected response.
	ErrServiceUnavailable = errors.New("governance decision log unavailable")
)

// Client verifies governance decisions against governance-decision-log-svc.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new governance-log verification client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

type decisionResponse struct {
	DecisionID    string `json:"decision_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	Outcome       string `json:"outcome"`
}

// VerifyGranted fetches decisionID from governance-decision-log-svc scoped
// to tenantID, and confirms it was GRANTED for actionType against
// legalEntityID. Fails closed: unreachable service, non-200 response, a
// decode failure, or any field mismatch all result in a non-nil error —
// activation must never proceed on an ambiguous verification result.
func (c *Client) VerifyGranted(ctx context.Context, tenantID, decisionID, legalEntityID, actionType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions/"+decisionID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrDecisionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ErrServiceUnavailable
	}

	var d decisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return ErrServiceUnavailable
	}

	if d.Outcome != "GRANTED" || d.LegalEntityID != legalEntityID || d.ActionType != actionType {
		return ErrDecisionNotGranted
	}
	return nil
}
