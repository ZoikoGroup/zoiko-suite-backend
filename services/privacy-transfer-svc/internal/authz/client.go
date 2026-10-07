package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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
	tenantID := legalEntityID
	if tenantID == "" || tenantID == "00000000-0000-0000-0000-00000000f001" {
		tenantID = "11111111-1111-1111-1111-111111111111"
	}

	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"tenant_id":       tenantID,
		"action_type":     actionType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Actor-Subject-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Request-Id", uuid.New().String())
	req.Header.Set("X-Correlation-Id", uuid.New().String())
	req.Header.Set("X-Source-Channel", "web")
	req.Header.Set("Idempotency-Key", uuid.New().String())

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
