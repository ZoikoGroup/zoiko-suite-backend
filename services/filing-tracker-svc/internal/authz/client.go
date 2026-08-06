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

// ErrAuthzServiceUnavailable is returned when authorization-svc could not be
// reached or did not respond with a usable decision. Callers must treat this
// as a denial (fail closed).
var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

// ErrAuthorizationDenied is returned when authorization-svc explicitly denied
// the requested action.
var ErrAuthorizationDenied = errors.New("authorization denied")

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType against legalEntityID. It fails CLOSED: any transport error,
// non-200 response, or decision other than "GRANTED" results in a non-nil
// error, and the caller must refuse the action.
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
