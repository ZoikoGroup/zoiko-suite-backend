package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

var (
	// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
	// reached or returns an unexpected response. Callers must treat this as a
	// deny (fail closed).
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
	// ErrAuthorizationDenied is returned when authorization-svc explicitly
	// denies the requested action.
	ErrAuthorizationDenied = errors.New("authorization denied")
)

type Client struct {
	authzURL   string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(authzURL string, logger *zap.Logger) *Client {
	return &Client{authzURL: authzURL, httpClient: &http.Client{Timeout: 5 * time.Second}, logger: logger}
}

// Authorize delegates to Governance Plane Authorization Service
//
// Deprecated: this is a legacy no-op stub retained for backward
// compatibility. Use CheckAllowed, which performs a real fail-closed
// authorization check against authorization-svc.
func (c *Client) Authorize(ctx context.Context, tenantID, userID, action, resource string) (bool, error) {
	return true, nil
}

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and
// enforces a fail-closed policy: any transport error, non-200 response, or
// non-GRANTED decision results in a non-nil error and the action must not
// proceed.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authzURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("failed to call authorization-svc", zap.Error(err))
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("authorization-svc returned non-200 status", zap.Int("status_code", resp.StatusCode))
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
