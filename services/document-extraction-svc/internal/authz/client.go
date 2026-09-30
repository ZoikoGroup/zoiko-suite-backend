package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Client struct {
	baseURL string
	logger  *zap.Logger
	client  *http.Client
}

var ErrAuthorizationDenied = &AuthorizationError{Message: "authorization denied"}

type AuthorizationError struct {
	Message string
}

func (e *AuthorizationError) Error() string {
	return e.Message
}

func NewClient(baseURL string, logger *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func NewClientWithHTTPClient(baseURL string, logger *zap.Logger, httpClient *http.Client) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
		client:  httpClient,
	}
}

func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	url := c.baseURL + "/v1/authorization/check"
	body := map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("authorization call failed", zap.Error(err))
		return ErrAuthorizationDenied
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return ErrAuthorizationDenied
	}
	if resp.StatusCode != http.StatusOK {
		c.logger.Error("authorization service error", zap.Int("status", resp.StatusCode))
		return ErrAuthorizationDenied
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.logger.Error("failed to decode authorization response", zap.Error(err))
		return ErrAuthorizationDenied
	}

	if result["decision_outcome"] != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
