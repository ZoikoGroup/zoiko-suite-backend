package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SpendControlsClient calls spend-controls-svc's real POST /v1/spend-checks
// endpoint synchronously. A procurement case is never created without a real
// spend-check outcome from this call — see domain.ErrSpendControlsUnavailable.
type SpendControlsClient struct {
	baseURL string
	http    *http.Client
}

func NewSpendControlsClient(baseURL string) *SpendControlsClient {
	return &SpendControlsClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

type SpendCheckResult struct {
	DecisionOutcome string `json:"decision_outcome"` // ALLOWED, BLOCKED
	DecisionBasis   string `json:"decision_basis"`
}

// SubmitCheck reuses the caller's own correlation_id as the spend-check's
// idempotency key — a retried procurement-case create request will therefore
// also replay the same spend-check decision at spend-controls-svc, rather
// than re-evaluating consumption a second time.
func (c *SpendControlsClient) SubmitCheck(ctx context.Context, tenantID, principalID, legalEntityID, category, currencyCode, correlationID, sourceReference string, amount float64) (*SpendCheckResult, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"legal_entity_id":  legalEntityID,
		"category":         category,
		"amount":           amount,
		"currency_code":    currencyCode,
		"source_reference": sourceReference,
		"correlation_id":   correlationID,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/spend-checks", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spend-controls-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spend-controls-svc returned %d for POST /v1/spend-checks", resp.StatusCode)
	}

	var result SpendCheckResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode spend-controls-svc response: %w", err)
	}
	return &result, nil
}
