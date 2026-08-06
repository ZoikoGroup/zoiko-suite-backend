package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Client talks to the Governance Plane authorization-svc.
// No domain service self-authorizes a material action (per doctrine.md).
type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be reached
// or does not respond with a usable decision. Callers must treat this as a denial
// (fail closed) rather than allowing the action to proceed.
var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

// ErrAuthorizationDenied is returned when authorization-svc explicitly denies the action.
var ErrAuthorizationDenied = errors.New("authorization denied")

// CheckAllowed calls authorization-svc's real contract: POST /v1/authorize with
// {"principal_id", "legal_entity_id", "action_type"}. The endpoint always answers
// HTTP 200 with a decision_outcome of "GRANTED" or "DENIED" in the body. Any failure
// to reach the service, a non-200 response, or a decision other than "GRANTED" is
// treated as not authorized (fail closed).
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}
	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
