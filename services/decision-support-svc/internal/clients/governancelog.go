// Package clients holds this service's HTTP clients for
// governance-decision-log-svc.
//
// Two calls are made:
//
//   - ListPriorDecisions (GET /v1/decisions) — synchronous, on the create
//     path, to ground a recommendation in real precedent. Any failure
//     degrades the recommendation to NO_HISTORY; it never fails the request.
//   - RecordDecision (POST /v1/decisions) — asynchronous, from the outbox
//     relay, to append each human ACCEPT/REJECT disposition to the platform's
//     immutable decision ledger.
package clients

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	svcenvelope "zoiko.io/decision-support-svc/internal/envelope"
)

// GovernanceLogClient talks to governance-decision-log-svc.
//
// How the decision log is read (D2-2.F07, 11 Nov 2026). The read used to
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
// decision log does not require a verified identity), e.g.
// "http://governance-decision-log-svc:8083".
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

// PriorDecision is the one field of a governance decision the precedent
// engine needs.
type PriorDecision struct {
	Outcome string `json:"outcome"` // GRANTED, DENIED, ESCALATED, ...
}

// HistoryQuery identifies the request and the precedent being asked for.
//
// The read is made as this service's own principal (see GovernanceLogClient);
// PrincipalID records the human caller for the service's own audit trail and
// is not sent. CorrelationID and RequestID carry the caller's trace.
type HistoryQuery struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
	RequestID     string
	LegalEntityID string
	ActionType    string
	Limit         int
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ListPriorDecisions returns up to limit most-recent governance decisions
// for the given legal entity + action type. The correlation id comes from the
// request's envelope when there is one.
func (c *GovernanceLogClient) ListPriorDecisions(ctx context.Context, tenantID, legalEntityID, actionType string, limit int) ([]PriorDecision, error) {
	return c.ListPriorDecisionsFor(ctx, HistoryQuery{
		TenantID: tenantID, LegalEntityID: legalEntityID, ActionType: actionType, Limit: limit,
	})
}

// ListPriorDecisionsFor is ListPriorDecisions carrying the caller's trace.
func (c *GovernanceLogClient) ListPriorDecisionsFor(ctx context.Context, q HistoryQuery) ([]PriorDecision, error) {
	query := url.Values{}
	query.Set("entity", q.LegalEntityID)
	query.Set("action", q.ActionType)
	query.Set("limit", strconv.Itoa(q.Limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	correlation := q.CorrelationID
	if correlation == "" {
		if env, ok := svcenvelope.FromContext(ctx); ok {
			correlation = env.CorrelationID
		}
	}
	if correlation == "" {
		correlation = newRequestID()
	}
	requestID := q.RequestID
	if requestID == "" {
		requestID = newRequestID()
	}
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("X-Tenant-Id", q.TenantID)
	h.Set("X-Principal-Id", c.principalID)
	h.Set("X-Request-Id", requestID)
	h.Set("X-Correlation-ID", correlation)
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", GovernanceLogPurpose)
	if q.LegalEntityID != "" {
		h.Set("X-Legal-Entity-Id", q.LegalEntityID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("governance-decision-log-svc unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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

// ErrDecisionRefusedPermanently means the ledger answered 4xx: the record
// will never be accepted as sent (e.g. a different decision already holds
// this decision_id). The relay parks it rather than retrying forever.
var ErrDecisionRefusedPermanently = errors.New("governance-decision-log-svc refused the decision permanently")

// DecisionRecord is one human disposition to append to the ledger. It is
// stored verbatim as the outbox payload, so the relay needs nothing else.
type DecisionRecord struct {
	// Envelope.
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"` // the human who decided
	CorrelationID string `json:"correlation_id"`

	// Body fields of governance-decision-log-svc's POST /v1/decisions.
	DecisionID        string          `json:"decision_id"`
	ActionType        string          `json:"action_type"`
	Outcome           string          `json:"outcome"`
	RuleBasis         string          `json:"rule_basis"`
	ActionSubjectType string          `json:"action_subject_type"`
	ActionSubjectID   string          `json:"action_subject_id"`
	EvaluationContext json.RawMessage `json:"evaluation_context"`
	DecidedAt         time.Time       `json:"decided_at"`
}

// RecordDecision appends rec to the ledger. decision_id doubles as the
// Idempotency-Key, so a redelivery after a lost response is a no-op there.
func (c *GovernanceLogClient) RecordDecision(ctx context.Context, rec DecisionRecord) error {
	body, err := json.Marshal(map[string]any{
		"decision_id":         rec.DecisionID,
		"tenant_id":           rec.TenantID,
		"legal_entity_id":     rec.LegalEntityID,
		"action_type":         rec.ActionType,
		"outcome":             rec.Outcome,
		"rule_basis":          rec.RuleBasis,
		"correlation_id":      rec.CorrelationID,
		"action_subject_type": rec.ActionSubjectType,
		"action_subject_id":   rec.ActionSubjectID,
		"evaluation_context":  rec.EvaluationContext,
		"decided_at":          rec.DecidedAt,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", rec.TenantID)
	req.Header.Set("X-Principal-Id", rec.ActorID)
	req.Header.Set("X-Legal-Entity-Id", rec.LegalEntityID)
	req.Header.Set("X-Correlation-ID", rec.CorrelationID)
	req.Header.Set("X-Request-Id", "decision-support-relay-"+rec.DecisionID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "ai_output_disposition_evidence")
	req.Header.Set("Idempotency-Key", rec.DecisionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("governance-decision-log-svc unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrDecisionRefusedPermanently, resp.StatusCode)
	default:
		return fmt.Errorf("governance-decision-log-svc returned %d for POST /v1/decisions", resp.StatusCode)
	}
}
