package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrAuthorizationDenied indicates authorization-svc explicitly denied the action.
	ErrAuthorizationDenied = errors.New("authorization denied")
	// ErrAuthzServiceUnavailable indicates authorization-svc could not be reached or
	// returned an unexpected response. Callers must treat this as a denial (fail closed).
	ErrAuthzServiceUnavailable = errors.New("authorization service unavailable")
)

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

type checkAllowedRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type checkAllowedResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and fails closed:
// any transport error, non-200 response, or decision other than GRANTED results in an error.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(checkAllowedRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
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

	var res checkAllowedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrAuthzServiceUnavailable
	}
	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
