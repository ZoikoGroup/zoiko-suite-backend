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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GovernanceLogClient talks to governance-decision-log-svc.
type GovernanceLogClient struct {
	baseURL string
	http    *http.Client
}

// NewGovernanceLogClient builds a client for baseURL, e.g.
// "http://governance-decision-log-svc:8083".
func NewGovernanceLogClient(baseURL string) *GovernanceLogClient {
	return &GovernanceLogClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PriorDecision is the one field of a governance decision the precedent
// engine needs.
type PriorDecision struct {
	Outcome string `json:"outcome"` // GRANTED, DENIED, ESCALATED, ...
}

// HistoryQuery identifies the caller and the precedent being asked for.
//
// The caller's principal and trace are forwarded: governance-decision-log-svc
// authorizes ledger reads on GOVERNANCE_DECISION_READ and refuses a read
// with no X-Principal-Id once DECISION_LOG_READ_AUTHZ=enforce (its config
// names this service as one of the callers that used to send none).
type HistoryQuery struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
	RequestID     string
	LegalEntityID string
	ActionType    string
	Limit         int
}

// ListPriorDecisions returns up to q.Limit most-recent governance decisions
// for the given legal entity + action type.
func (c *GovernanceLogClient) ListPriorDecisions(ctx context.Context, q HistoryQuery) ([]PriorDecision, error) {
	query := url.Values{}
	query.Set("entity", q.LegalEntityID)
	query.Set("action", q.ActionType)
	query.Set("limit", strconv.Itoa(q.Limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", q.TenantID)
	req.Header.Set("X-Principal-Id", q.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", q.LegalEntityID)
	req.Header.Set("X-Correlation-ID", q.CorrelationID)
	req.Header.Set("X-Request-Id", q.RequestID)
	req.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("governance-decision-log-svc unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("governance-decision-log-svc returned %d for GET /v1/decisions", resp.StatusCode)
	}

	var decisions []PriorDecision
	if err := json.NewDecoder(resp.Body).Decode(&decisions); err != nil {
		return nil, fmt.Errorf("decode governance-decision-log-svc response: %w", err)
	}
	return decisions, nil
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
