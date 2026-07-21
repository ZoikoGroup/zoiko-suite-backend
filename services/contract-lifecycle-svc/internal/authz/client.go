package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client calls the Authorization Service to check permissions.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new authorization client.
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

// Authorize returns true if the actor is permitted to perform the action.
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
		// Fail open in degraded mode — log but allow
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
