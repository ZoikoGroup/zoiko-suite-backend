// Package policyclient fetches a specific PolicyVersion by ID from
// policy-svc — the "as of" lookup doc7's replay doctrine needs. Distinct
// from a normal evaluation call: a replay must re-evaluate against the
// EXACT version a past decision actually used, whether or not that
// version is still ACTIVE today, so this deliberately calls
// GET /v1/policy-versions/{id} rather than policy-svc's Evaluate (which
// always resolves whatever is currently applicable).
package policyclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrPolicyVersionNotFound is returned when policy-svc has no version
// with the given ID.
var ErrPolicyVersionNotFound = fmt.Errorf("policy version not found")

// ErrPolicyServiceUnavailable is returned when policy-svc cannot be
// reached or answers with anything other than 200/404. Replay must fail
// closed — a replay that cannot fetch the original version proves
// nothing and must not be recorded as if it did.
var ErrPolicyServiceUnavailable = fmt.Errorf("policy-svc unavailable")

// PolicyVersion is the subset of policy-svc's PolicyVersion this client
// needs — just enough to re-run the evaluation, not a full mirror of
// policy-svc's own domain type.
type PolicyVersion struct {
	PolicyVersionID string          `json:"policy_version_id"`
	RulePayload     json.RawMessage `json:"rule_payload"`
}

type Client interface {
	// GetPolicyVersion fetches one version in the given tenant's scope.
	//
	// tenantID is forwarded as X-Tenant-Id. policy-svc scopes the lookup to it
	// (a version is visible if it is global or belongs to that tenant) and now
	// REFUSES a request that carries no scope at all — an omitted header used to
	// fall back to an unscoped lookup there, so any tenant's version could be
	// read by id. A non-uuid tenant such as this service's "GLOBAL" sentinel
	// narrows the lookup to global versions, which is what a globally-evaluated
	// decision was replayed against anyway.
	GetPolicyVersion(ctx context.Context, tenantID, policyVersionID string) (*PolicyVersion, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *HTTPClient) GetPolicyVersion(ctx context.Context, tenantID, policyVersionID string) (*PolicyVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/policy-versions/"+policyVersionID, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyServiceUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyServiceUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPolicyVersionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: unexpected status %d", ErrPolicyServiceUnavailable, resp.StatusCode)
	}

	var v PolicyVersion
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyServiceUnavailable, err)
	}
	return &v, nil
}

// ParseRuleBasis splits a governance_decisions.rule_basis value of the
// form "policyCode:policyVersionID" (see policy-svc's
// evaluateApprovalThreshold) into its two parts. Splits on the LAST
// colon, since policy_version_id is always a UUID (no colons) and
// policy_code is free text an admin could theoretically include a colon
// in — splitting from the end is the only robust choice.
func ParseRuleBasis(ruleBasis string) (policyCode, policyVersionID string, ok bool) {
	idx := strings.LastIndex(ruleBasis, ":")
	if idx < 0 || idx == len(ruleBasis)-1 {
		return "", "", false
	}
	return ruleBasis[:idx], ruleBasis[idx+1:], true
}
