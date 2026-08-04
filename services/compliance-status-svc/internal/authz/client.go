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

// Client talks to the Governance Plane authorization-svc.
// No domain service self-authorizes a material action (per doctrine.md).
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

// Sentinel errors for CheckAllowed. Any failure/ambiguity must fail closed:
// callers must never treat a non-nil error as "allowed".
var (
	// ErrAuthorizationDenied means authorization-svc explicitly denied the action.
	ErrAuthorizationDenied = errors.New("authorization denied")
	// ErrAuthzServiceUnavailable means authorization-svc could not be reached or
	// returned an unexpected response; the caller must fail closed.
	ErrAuthzServiceUnavailable = errors.New("authorization service unavailable")
)

type checkAllowedRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type checkAllowedResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed asks authorization-svc whether principalID may perform actionType
// on legalEntityID. It fails closed: any transport error, non-200 response, or
// decoding failure results in ErrAuthzServiceUnavailable, and only an explicit
// "GRANTED" decision_outcome results in a nil error.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(checkAllowedRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
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
