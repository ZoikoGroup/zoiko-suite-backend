// Package governancelog verifies that a purchase request approval is backed by a
// real, GRANTED governance decision.
package governancelog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var (
	ErrDecisionNotFound   = errors.New("governance decision not found")
	ErrDecisionNotGranted = errors.New("governance decision was not granted for this action")
	ErrServiceUnavailable = errors.New("governance decision log unavailable")
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

type decisionResponse struct {
	DecisionID    string `json:"decision_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	Outcome       string `json:"outcome"`
}

func (c *Client) VerifyGranted(ctx context.Context, tenantID, decisionID, legalEntityID, actionType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions/"+decisionID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrDecisionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ErrServiceUnavailable
	}

	var d decisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return ErrServiceUnavailable
	}

	if d.Outcome != "GRANTED" || d.LegalEntityID != legalEntityID || d.ActionType != actionType {
		return ErrDecisionNotGranted
	}
	return nil
}
