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

// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
// reached, or responds with anything other than a well-formed 200. Callers
// must treat this as a denial — fail closed, never silently allow.
var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

// ErrAuthorizationDenied is returned when authorization-svc explicitly
// denies the requested action.
var ErrAuthorizationDenied = errors.New("authorization denied")

type Client struct {
	authzURL   string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(authzURL string, logger *zap.Logger) *Client {
	return &Client{
		authzURL:   authzURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
	}
}

// Authorize is retained for backward compatibility but is unused anywhere
// in this service — it never called authorization-svc and always granted.
func (c *Client) Authorize(ctx context.Context, tenantID, userID, action, resource string) (bool, error) {
	// Delegated to Governance Plane Authorization Service
	return true, nil
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType within legalEntityID. It fails closed: any transport error,
// non-200 status, or undecodable body is treated as
// ErrAuthzServiceUnavailable, never as an implicit grant.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return ErrAuthzServiceUnavailable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authzURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			c.logger.Error("authorization-svc unreachable — failing closed", zap.Error(err))
		}
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ErrAuthzServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
