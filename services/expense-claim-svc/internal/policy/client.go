// Package policy calls the real policy-svc for an APPROVAL_THRESHOLD
// evaluation of a claim's total amount. A 404 ("no_applicable_policy")
// is reported as domain.ErrNoApplicablePolicy and an unreachable/erroring
// service as domain.ErrPolicyServiceUnavailable; neither is turned into a
// permissive result here. The handler fails closed for controlled
// categories and is the only place that may let an unassessed claim
// through (see internal/handler).
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
)

// Caller is the verified identity of the request being evaluated, forwarded
// to policy-svc. policy-svc takes the actor from X-Principal-Id, authorizes it
// (POLICY_EVALUATE) and checks the signed envelope in Authorization against
// those headers; a body actor is ignored there.
type Caller struct {
	PrincipalID   string
	Authorization string
	RequestID     string
	CorrelationID string
}

type Client interface {
	EvaluateApprovalThreshold(ctx context.Context, caller Caller, tenantID, legalEntityID string, amount float64) (result string, policyVersionID string, err error)
}

type evaluateRequest struct {
	PolicyType    string          `json:"policy_type"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActionContext json.RawMessage `json:"action_context,omitempty"`
	DecisionID    string          `json:"decision_id"`
}

type evaluateResponse struct {
	Result          string `json:"result"`
	PolicyVersionID string `json:"policy_version_id"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) EvaluateApprovalThreshold(ctx context.Context, caller Caller, tenantID, legalEntityID string, amount float64) (string, string, error) {
	actionContext, _ := json.Marshal(map[string]float64{"amount": amount})
	body := evaluateRequest{
		PolicyType: "APPROVAL_THRESHOLD", TenantID: tenantID, LegalEntityID: legalEntityID,
		ActionContext: actionContext, DecisionID: uuid.New().String(),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/policies/evaluate", bytes.NewReader(payload))
	if err != nil {
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	if caller.Authorization != "" {
		req.Header.Set("Authorization", caller.Authorization)
	}
	requestID := caller.RequestID
	if requestID == "" {
		requestID = uuid.New().String()
	}
	req.Header.Set("X-Request-Id", requestID)
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", caller.CorrelationID)
	}
	req.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("policy-svc unreachable — failing closed", zap.Error(err))
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// "no_applicable_policy": the caller decides whether that is fatal
		// (controlled categories); this client never invents WITHIN_THRESHOLD.
		return "", "", domain.ErrNoApplicablePolicy
	}
	if resp.StatusCode != http.StatusOK {
		// 401/403 is the caller lacking POLICY_EVALUATE, not an outage; the
		// status is logged so the two can be told apart.
		c.log.Error("unexpected response from policy-svc — failing closed", zap.Int("status", resp.StatusCode))
		return "", "", domain.ErrPolicyServiceUnavailable
	}

	var out evaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Result == "" {
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	return out.Result, out.PolicyVersionID, nil
}
