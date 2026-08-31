// Package policy calls the real policy-svc for an APPROVAL_THRESHOLD
// evaluation of an authorization's net amount — AP-10's own "delegated
// signing limit" (negative-path scenario #2). Same integration AP-07 and
// AP-09 already use. A 404 ("no_applicable_policy") means no threshold
// policy has been configured yet; treated as WITHIN_THRESHOLD, per
// policy-svc's own documented stance that it will not guess fail-open/
// fail-closed for an unconfigured policy.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

type Client interface {
	EvaluateApprovalThreshold(ctx context.Context, principalID, tenantID, legalEntityID string, amount float64) (result string, policyVersionID string, err error)
}

type evaluateRequest struct {
	PolicyType             string          `json:"policy_type"`
	TenantID               string          `json:"tenant_id,omitempty"`
	LegalEntityID          string          `json:"legal_entity_id,omitempty"`
	ActionContext          json.RawMessage `json:"action_context,omitempty"`
	EvaluatedByPrincipalID string          `json:"evaluated_by_principal_id"`
	DecisionID             string          `json:"decision_id"`
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

func (c *HTTPClient) EvaluateApprovalThreshold(ctx context.Context, principalID, tenantID, legalEntityID string, amount float64) (string, string, error) {
	actionContext, _ := json.Marshal(map[string]float64{"amount": amount})
	body := evaluateRequest{
		PolicyType: "APPROVAL_THRESHOLD", TenantID: tenantID, LegalEntityID: legalEntityID,
		ActionContext: actionContext, EvaluatedByPrincipalID: principalID, DecisionID: uuid.New().String(),
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

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("policy-svc unreachable — failing closed", zap.Error(err))
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "WITHIN_THRESHOLD", "", nil
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from policy-svc — failing closed", zap.Int("status", resp.StatusCode))
		return "", "", domain.ErrPolicyServiceUnavailable
	}

	var out evaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Result == "" {
		return "", "", domain.ErrPolicyServiceUnavailable
	}
	return out.Result, out.PolicyVersionID, nil
}
