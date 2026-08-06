package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

var (
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
	ErrAuthorizationDenied     = errors.New("authorization denied")
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Authorize is a legacy no-op stub retained for backward compatibility.
// It is not used by any handler; CheckAllowed enforces the real fail-closed
// authorization contract and should be used for all write actions.
func (c *Client) Authorize(ctx context.Context, tenantID, action, resource string) (bool, error) {
	return true, nil
}

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and
// enforces fail-closed behavior: any transport error, non-200 response, or
// decision other than "GRANTED" results in the action being refused.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}

	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrAuthzServiceUnavailable
	}

	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}

	return nil
}
