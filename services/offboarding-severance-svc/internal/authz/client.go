package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"zoiko.io/offboarding-severance-svc/internal/domain"
)

type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// CheckAllowed calls authorization-svc's real /v1/authorize endpoint.
// Fails closed: any transport error or non-200 response is reported as
// domain.ErrAuthzServiceUnavailable, never treated as an implicit allow.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
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
		return domain.ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var res struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return domain.ErrAuthzServiceUnavailable
	}
	if !res.Allowed {
		return domain.ErrAuthorizationDenied
	}
	return nil
}
