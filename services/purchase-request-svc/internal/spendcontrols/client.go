// Package spendcontrols calls spend-controls-svc's POST /v1/spend-checks — the
// budget / spend-policy check made when a requisition is submitted (spec §5:
// "budget/policy service unavailable may block controlled categories").
//
// A check that ALLOWS records consumption at spend-controls-svc, and is
// idempotent on correlation_id, so the caller derives that id from
// (requisition, version, category): a retried submit replays the same decision
// instead of consuming the budget twice.
package spendcontrols

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrUnavailable means no decision could be obtained. Callers fail closed for
// controlled categories.
var ErrUnavailable = errors.New("spend-controls-svc unavailable")

// CheckInput is one category-level check.
type CheckInput struct {
	TenantID        string
	PrincipalID     string
	LegalEntityID   string
	Category        string
	CurrencyCode    string
	Amount          float64
	CorrelationID   string
	SourceReference string
}

// Result is the decision.
type Result struct {
	Outcome string `json:"decision_outcome"` // ALLOWED | BLOCKED
	Basis   string `json:"decision_basis"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	Check(ctx context.Context, in CheckInput) (*Result, error)
}

// HTTPClient implements Client.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient builds a client for baseURL (e.g. http://spend-controls-svc:8131).
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) Check(ctx context.Context, in CheckInput) (*Result, error) {
	body, _ := json.Marshal(map[string]any{
		"legal_entity_id":  in.LegalEntityID,
		"category":         in.Category,
		"amount":           in.Amount,
		"currency_code":    in.CurrencyCode,
		"source_reference": in.SourceReference,
		"correlation_id":   in.CorrelationID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/spend-checks", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", in.TenantID)
	req.Header.Set("X-Principal-Id", in.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", in.LegalEntityID)
	req.Header.Set("X-Correlation-ID", in.CorrelationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Idempotency-Key", "spendcheck:"+in.CorrelationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: spend-controls-svc returned %d", ErrUnavailable, resp.StatusCode)
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: malformed response: %v", ErrUnavailable, err)
	}
	if out.Outcome != "ALLOWED" && out.Outcome != "BLOCKED" {
		return nil, fmt.Errorf("%w: unrecognised decision %q", ErrUnavailable, out.Outcome)
	}
	return &out, nil
}
