package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, action, resource string) error
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

type CheckResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

func (c *Client) CheckAllowed(ctx context.Context, principalID, action, resource string) error {
	if c.baseURL == "" || principalID == "" {
		return nil
	}

	url := fmt.Sprintf("%s/v1/authz/check", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	tenantID := middleware.GetTenantID(ctx)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var res CheckResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err == nil && !res.Allowed {
			return fmt.Errorf("authorization denied for action %s: %s", action, res.Reason)
		}
	}

	return nil
}
