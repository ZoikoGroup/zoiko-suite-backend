package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// GovernanceLogClient calls governance-decision-log-svc's real
// GET /v1/decisions endpoint synchronously — the platform's own immutable
// record of every governance decision ever made — to build recommendations
// from real historical precedent rather than an invented heuristic.
type GovernanceLogClient struct {
	baseURL string
	http    *http.Client
}

func NewGovernanceLogClient(baseURL string) *GovernanceLogClient {
	return &GovernanceLogClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

type PriorDecision struct {
	Outcome string `json:"outcome"` // GRANTED, DENIED, ESCALATED
}

// ListPriorDecisions returns up to limit most-recent governance decisions
// for the given legal entity + action type.
func (c *GovernanceLogClient) ListPriorDecisions(ctx context.Context, tenantID, legalEntityID, actionType string, limit int) ([]PriorDecision, error) {
	q := url.Values{}
	q.Set("entity", legalEntityID)
	q.Set("action", actionType)
	q.Set("limit", fmt.Sprintf("%d", limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("governance-decision-log-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("governance-decision-log-svc returned %d for GET /v1/decisions", resp.StatusCode)
	}

	var decisions []PriorDecision
	if err := json.NewDecoder(resp.Body).Decode(&decisions); err != nil {
		return nil, fmt.Errorf("decode governance-decision-log-svc response: %w", err)
	}
	return decisions, nil
}
