// Package decisionlog delivers policy evaluations to
// governance-decision-log-svc's POST /v1/decisions — the governance ledger
// half of the "preserve evaluation basis for governed decisions" evidence
// obligation (Doc 03 §8.1). The other half, the evaluation's own evidence
// row, is written by Evaluate in policy-svc's database before it answers;
// this client is called by the evidence relay (internal/evidence), never
// on the request path.
//
// The ledger writer is policy-svc's own service principal (decision D-02,
// 14 Oct 2026): it alone needs GOVERNANCE_DECISION_RECORD. The user whose
// action was evaluated travels in evaluation_context.requested_by_principal_id.
package decisionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	svcenvelope "zoiko.io/policy-svc/internal/envelope"
)

// PurposeContext is sent as X-Purpose-Context. governance-decision-log-svc's
// envelope contract requires it on every append (whether it should is held as
// that service's open decision D-06), so every write names its purpose.
const PurposeContext = "policy_evaluation_evidence"

// PlatformScope is governance-decision-log-svc's (and authorization-svc's)
// literal for "not scoped to one legal entity". It is the one non-UUID
// legal_entity_id that service accepts.
const PlatformScope = "PLATFORM"

// ErrPermanent marks a refusal that will not change on retry: the ledger
// already holds a different decision under this id or key. The relay parks
// such a record instead of retrying.
var ErrPermanent = errors.New("governance-decision-log-svc refused the record permanently")

// Record is one decision to append to the ledger.
type Record struct {
	// Envelope.
	TenantID       string
	LegalEntityID  string // a UUID, or PlatformScope
	PrincipalID    string // the ledger writer
	CorrelationID  string
	CausationID    string
	RequestID      string
	IdempotencyKey string

	// Body.
	DecisionID        string
	ActionType        string
	Outcome           string
	RuleBasis         string
	PolicyVersionID   string
	EvaluationContext json.RawMessage
	DecidedAt         time.Time
}

// Client appends decisions to the governance ledger.
type Client interface {
	RecordDecision(ctx context.Context, rec Record) error
}

// HTTPClient implements Client against a real governance-decision-log-svc.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://governance-decision-log-svc:8083".
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// createDecisionRequest mirrors governance-decision-log-svc's
// createDecisionRequest. tenant_id is deliberately omitted: that service
// files the decision under the verified X-Tenant-Id and refuses a body that
// names a different one (the "GLOBAL" sentinel this client used to send got
// 403 TENANT_SCOPE_MISMATCH on every global-scope evaluation).
type createDecisionRequest struct {
	DecisionID        string          `json:"decision_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	ActionType        string          `json:"action_type"`
	Outcome           string          `json:"outcome"`
	RuleBasis         string          `json:"rule_basis"`
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID     string          `json:"correlation_id"`
	PolicyVersionID   string          `json:"policy_version_id,omitempty"`
	DecidedAt         time.Time       `json:"decided_at"`
}

// RecordDecision POSTs rec with the full canonical envelope that
// governance-decision-log-svc's write-strict contract requires. 200 (replay)
// and 201 (recorded) are both success, which makes redelivery after a lost
// response safe.
func (c *HTTPClient) RecordDecision(ctx context.Context, rec Record) error {
	body, err := json.Marshal(createDecisionRequest{
		DecisionID:        rec.DecisionID,
		LegalEntityID:     rec.LegalEntityID,
		ActionType:        rec.ActionType,
		Outcome:           rec.Outcome,
		RuleBasis:         rec.RuleBasis,
		EvaluationContext: rec.EvaluationContext,
		CorrelationID:     rec.CorrelationID,
		PolicyVersionID:   rec.PolicyVersionID,
		DecidedAt:         rec.DecidedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("%w: marshal decision: %v", ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build decision request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", rec.TenantID)
	h.Set("X-Principal-Id", rec.PrincipalID)
	h.Set("X-Legal-Entity-Id", rec.LegalEntityID)
	h.Set("X-Correlation-ID", rec.CorrelationID)
	h.Set("X-Request-Id", rec.RequestID)
	h.Set("X-Source-Channel", "system")
	h.Set("Idempotency-Key", rec.IdempotencyKey)
	h.Set("X-Purpose-Context", PurposeContext)
	if rec.CausationID != "" {
		h.Set("X-Causation-Id", rec.CausationID)
	}

	// Forward the caller's canonical envelope for downstream traceability
	svcenvelope.ForwardTo(ctx, req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call governance-decision-log-svc: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusConflict:
		// A different decision already holds this id or key: sending the
		// same record again cannot succeed.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: %d: %s", ErrPermanent, resp.StatusCode, msg)
	default:
		// 400 (a contract mismatch between the two services' versions), 401
		// and 403 (writer grant missing), 5xx: a deploy, a grant or time
		// fixes these, and the relay rebuilds the record from the stored
		// evaluation on every attempt, so it retries with backoff rather than
		// give up on evidence.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("governance-decision-log-svc returned %d: %s", resp.StatusCode, msg)
	}
}
