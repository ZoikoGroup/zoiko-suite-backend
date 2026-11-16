package clients

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	svcenvelope "zoiko.io/decision-support-svc/internal/envelope"
)

// GovernanceLogClient calls governance-decision-log-svc's real
// GET /v1/decisions endpoint synchronously — the platform's own immutable
// record of every governance decision ever made — to build recommendations
// from real historical precedent rather than an invented heuristic.
//
// How the decision log is called (D2-2.F07, 11 Nov 2026). The read used to
// carry X-Tenant-Id alone and decoded the answer as a bare array, which the
// decision log has not returned since it gained paging ({"records": [...]}),
// so every recommendation silently degraded to NO_HISTORY. It now:
//   - decodes the page's records;
//   - reads as this service's own principal (GOVERNANCE_DECISION_LOG_PRINCIPAL_ID,
//     default svc-decision-support, granted GOVERNANCE_DECISION_READ): the
//     sample is an aggregate precedent count, not a person's read of the ledger;
//   - presents this service's workload certificate to the decision log's
//     :8450 listener (GOVERNANCE_DECISION_LOG_MTLS_URL), where the identity gate
//     admits it as a peer workload (IDENTITY_PEERS);
//   - sends the full input envelope with a read purpose (register D-04, D-06).
type GovernanceLogClient struct {
	baseURL     string
	http        *http.Client
	principalID string
}

// DefaultGovernanceLogPrincipalID is the principal this service reads as.
const DefaultGovernanceLogPrincipalID = "svc-decision-support"

// GovernanceLogPurpose is the purpose sent on every read.
const GovernanceLogPurpose = "decision_support_precedent"

// NewGovernanceLogClient is the plain-HTTP client (local runs where the
// decision log does not require a verified identity).
func NewGovernanceLogClient(baseURL string) *GovernanceLogClient {
	return NewGovernanceLogClientWithHTTPClient(baseURL, &http.Client{Timeout: 5 * time.Second}, DefaultGovernanceLogPrincipalID)
}

// NewGovernanceLogClientWithHTTPClient uses httpClient — the mTLS client
// presenting this service's workload certificate — and reads as principalID.
func NewGovernanceLogClientWithHTTPClient(baseURL string, httpClient *http.Client, principalID string) *GovernanceLogClient {
	if strings.TrimSpace(principalID) == "" {
		principalID = DefaultGovernanceLogPrincipalID
	}
	return &GovernanceLogClient{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient, principalID: principalID}
}

type PriorDecision struct {
	Outcome string `json:"outcome"` // GRANTED, DENIED, ESCALATED
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
	h := req.Header
	h.Set("X-Tenant-Id", tenantID)
	h.Set("X-Principal-Id", c.principalID)
	h.Set("X-Request-Id", newRequestID())
	correlation := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		correlation = env.CorrelationID
	}
	if correlation == "" {
		correlation = newRequestID()
	}
	h.Set("X-Correlation-ID", correlation)
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", GovernanceLogPurpose)
	if legalEntityID != "" {
		h.Set("X-Legal-Entity-Id", legalEntityID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("governance-decision-log-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("governance-decision-log-svc returned %d for GET /v1/decisions", resp.StatusCode)
	}

	var page struct {
		Records *[]PriorDecision `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode governance-decision-log-svc response: %w", err)
	}
	if page.Records == nil {
		return nil, fmt.Errorf("decode governance-decision-log-svc response: no records array")
	}
	return *page.Records, nil
}
