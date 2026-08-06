package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrAuthorizationDenied is returned when authorization-svc explicitly denies
// the requested action.
var ErrAuthorizationDenied = errors.New("authorization denied")

// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
// reached, or responds with anything other than a clean 200 decision. Callers
// must treat this as a denial (fail closed).
var ErrAuthzServiceUnavailable = errors.New("authorization service unavailable")

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

type authorizeRequest struct {
	TenantID   string `json:"tenant_id"`
	ActorID    string `json:"actor_id"`
	Action     string `json:"action"`
	ResourceID string `json:"resource_id"`
}

type authorizeResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func (c *Client) Authorize(ctx context.Context, tenantID, actorID, action, resourceID string) (bool, error) {
	body, _ := json.Marshal(authorizeRequest{
		TenantID:   tenantID,
		ActorID:    actorID,
		Action:     action,
		ResourceID: resourceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("authz service returned %d", resp.StatusCode)
	}
	var res authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false, err
	}
	return res.Allowed, nil
}

// checkAllowedRequest is the request body accepted by authorization-svc's
// real contract: POST /v1/authorize.
type checkAllowedRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// checkAllowedResponse is the response body returned by authorization-svc.
// It always responds HTTP 200; the decision lives in decision_outcome.
type checkAllowedResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType against legalEntityID. It fails closed: any transport error,
// non-200 response, decode error, or a decision other than "GRANTED" results
// in a non-nil error.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(checkAllowedRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", strings.NewReader(string(reqBody)))
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
