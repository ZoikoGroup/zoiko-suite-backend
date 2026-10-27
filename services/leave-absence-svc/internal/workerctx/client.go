// Package workerctx asks employee-master-svc (WFP-01) whether a worker may be
// paid by a legal entity on a date. It fails closed: an unreachable answer is
// "cannot answer", never "valid".
package workerctx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/leave-absence-svc/internal/domain"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: c}
}

// Caller is forwarded so the callee authorizes and audits the real caller.
type Caller struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
}

// Result is employee-master-svc's verdict for one worker.
type Result struct {
	Valid   bool
	Reasons []string
}

func (c *Client) Validate(ctx context.Context, caller Caller, workerID, legalEntityID, asOf string) (Result, error) {
	q := url.Values{"worker_id": {workerID}, "legal_entity_id": {legalEntityID}, "as_of": {asOf}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/wfp/worker-financial-profiles/validate-context?"+q.Encode(), nil)
	if err != nil {
		return Result{}, domain.ErrWorkerContextUnavailable
	}
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", caller.LegalEntityID)
	req.Header.Set("X-Correlation-ID", caller.CorrelationID)

	resp, err := c.client.Do(req)
	if err != nil {
		return Result{}, domain.ErrWorkerContextUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, domain.ErrWorkerContextUnavailable
	}
	var body struct {
		Valid   bool     `json:"valid"`
		Reasons []string `json:"reasons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Result{}, domain.ErrWorkerContextUnavailable
	}
	return Result{Valid: body.Valid, Reasons: body.Reasons}, nil
}
