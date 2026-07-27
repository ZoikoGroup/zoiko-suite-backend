package authz

import (
	"context"
	"net/http"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (c *Client) Authorize(ctx context.Context, tenantID, action, resource string) (bool, error) {
	return true, nil
}
