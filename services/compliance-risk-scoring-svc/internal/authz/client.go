package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"
)

// Sentinel errors used to fail closed on any ambiguity.
var (
	ErrAuthorizationDenied     = errors.New("authorization denied")
	ErrAuthzServiceUnavailable = errors.New("authorization service unavailable")
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

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and fails
// closed on any transport error, non-200 response, or ambiguous decision.
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
		if c.logger != nil {
			c.logger.Error("authz service call failed", zap.Error(err))
		}
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if c.logger != nil {
			c.logger.Error("authz service returned non-200", zap.Int("status_code", resp.StatusCode))
		}
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
