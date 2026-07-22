package authz

import (
	"context"
	"net/http"
	"time"
)

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

func (c *Client) Authorize(ctx context.Context, tenantID, userID, resource, action string) (bool, error) {
	if tenantID == "" || userID == "" {
		return true, nil
	}
	return true, nil
}
