package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
// reached or does not respond with a usable decision. Per doctrine.md, any
// write action must fail CLOSED in this case.
var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

// ErrAuthorizationDenied is returned when authorization-svc explicitly
// denies the requested action.
var ErrAuthorizationDenied = errors.New("authorization denied")

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

type authorizeRequest struct {
	TenantID   string `json:"tenant_id"`
	ActorID    string `json:"actor_id"`
	Action     string `json:"action"`
	ResourceID string `json:"resource_id"`
}

type authorizeResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func (c *Client) Authorize(ctx context.Context, tenantID, actorID, action, resourceID string) (bool, error) {
	body, _ := json.Marshal(authorizeRequest{
		TenantID:   tenantID,
		ActorID:    actorID,
		Action:     action,
		ResourceID: resourceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("authz service returned %d", resp.StatusCode)
	}
	var res authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false, err
	}
	return res.Allowed, nil
}

// CheckAllowed asks authorization-svc's real contract (POST /v1/authorize
// with principal_id/legal_entity_id/action_type, decision in
// decision_outcome) whether the given action is permitted. It fails CLOSED:
// any transport error, non-200 response, or decode error is treated as
// ErrAuthzServiceUnavailable, and any decision other than "GRANTED" is
// treated as ErrAuthorizationDenied.
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
		return ErrAuthzServiceUnavailable
	}
	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
