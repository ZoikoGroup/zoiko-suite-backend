// Package transferregistry is a real HTTP client to privacy-transfer-svc (PRV-05),
// used to evaluate transfer conditions and authorization (§12.1, §13 step 4, §16-17).
package transferregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("privacy-transfer-svc unavailable")

// ErrMissingRequestContext is returned, without any network call, when the
// request context carries no correlation id or idempotency key. EvaluateTransfer
// is a mutating route on privacy-transfer-svc (§18.1): it refuses a call
// without both, so sending one would only produce a 400 the caller would then
// misread as an outage. The decision fails closed either way.
var ErrMissingRequestContext = errors.New("transfer call needs a correlation id and an idempotency key")

type requestContextKey struct{}

type requestContext struct{ correlationID, idempotencyKey string }

// WithRequestContext attaches the §18.1 context of the decision request that
// triggers a transfer evaluation: its X-Correlation-ID and its Idempotency-Key.
// The client derives the transfer call's own key from the latter.
func WithRequestContext(ctx context.Context, correlationID, idempotencyKey string) context.Context {
	return context.WithValue(ctx, requestContextKey{}, requestContext{
		correlationID:  strings.TrimSpace(correlationID),
		idempotencyKey: strings.TrimSpace(idempotencyKey),
	})
}

// TransferIdempotencyKey is the Idempotency-Key sent to privacy-transfer-svc for
// a decision request that carried callerKey. It is DETERMINISTIC: a retry of
// the same decision request (same key, same tenant and principal) repeats the
// same transfer key and body, so privacy-transfer-svc replays its original
// authorization instead of recording a second one, while a different decision
// request — a different key — gets its own transfer evaluation. The suffix keeps
// it distinct from the caller's key.
func TransferIdempotencyKey(callerKey string) string {
	return strings.TrimSpace(callerKey) + ":transfer"
}

type EvaluateTransferRequest struct {
	TenantID                string `json:"tenant_id,omitempty"`
	RelationshipID          string `json:"relationship_id"`
	TransferMechanismID     string `json:"transfer_mechanism_id"`
	DestinationJurisdiction string `json:"destination_jurisdiction,omitempty"`
	AssessmentRequired      bool   `json:"assessment_required"`
}

type TransferDecision struct {
	DecisionID              string     `json:"decision_id"`
	Result                  string     `json:"result"` // AUTHORIZED, CONDITIONAL, BLOCKED, REVIEW_REQUIRED
	ReasonCodes             []string   `json:"reason_codes"`
	TransferMechanismID     string     `json:"transfer_mechanism_id"`
	RelationshipID          string     `json:"relationship_id"`
	DestinationJurisdiction string     `json:"destination_jurisdiction,omitempty"`
	ExpiresAt               *time.Time `json:"expires_at,omitempty"`
}

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

// EvaluateTransfer calls POST /privacy/transfer-decisions in PRV-05.
func (c *Client) EvaluateTransfer(ctx context.Context, tenantID, principalID string, req *EvaluateTransferRequest) (*TransferDecision, error) {
	if req == nil {
		return nil, errors.New("transfer request is nil")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/privacy/transfer-decisions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// §18.1 context privacy-transfer-svc requires on every mutating call.
	rc, _ := ctx.Value(requestContextKey{}).(requestContext)
	if rc.correlationID == "" || rc.idempotencyKey == "" {
		return nil, ErrMissingRequestContext
	}
	httpReq.Header.Set("X-Correlation-ID", rc.correlationID)
	httpReq.Header.Set("Idempotency-Key", TransferIdempotencyKey(rc.idempotencyKey))
	if principalID != "" {
		httpReq.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		httpReq.Header.Set("X-Tenant-Id", tenantID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}

	var decision TransferDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return nil, ErrUnavailable
	}
	return &decision, nil
}
