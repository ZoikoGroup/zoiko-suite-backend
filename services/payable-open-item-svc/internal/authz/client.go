// Package authz is a real HTTP client to authorization-svc — including its
// dynamic own-object Segregation-of-Duties layer. CheckAllowedOwnObject
// supplies resource_owner_principal_id so a claimant attempting to decide
// their own expense claim is genuinely denied by authorization-svc's own
// SoD rule evaluation — the same feature and calling pattern first
// exercised by supplier-financial-profile-svc (AP-01) and reused by
// goods-service-receipt-svc (AP-04).
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

var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
var ErrAuthorizationDenied = errors.New("authorization denied")

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	return c.checkAllowed(ctx, principalID, legalEntityID, actionType, "")
}

func (c *Client) CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error {
	return c.checkAllowed(ctx, principalID, legalEntityID, actionType, resourceOwnerPrincipalID)
}

func (c *Client) checkAllowed(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error {
	body := map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	}
	if resourceOwnerPrincipalID != "" {
		body["resource_owner_principal_id"] = resourceOwnerPrincipalID
	}
	reqBody, _ := json.Marshal(body)
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
