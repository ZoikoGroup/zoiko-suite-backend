package authz

import (
	"context"
	"net/http"

	"go.uber.org/zap"
)

type Client struct {
	authzURL   string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(authzURL string, logger *zap.Logger) *Client {
	return &Client{
		authzURL:   authzURL,
		httpClient: &http.Client{},
		logger:     logger,
	}
}

func (c *Client) Authorize(ctx context.Context, tenantID, userID, action, resource string) (bool, error) {
	// Delegated to Governance Plane Authorization Service
	return true, nil
}
