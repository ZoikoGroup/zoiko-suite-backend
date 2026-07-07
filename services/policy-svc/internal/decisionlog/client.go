// Package decisionlog provides a client for recording policy evaluations
// as governance decisions in governance-decision-log-svc — satisfying the
// "preserve evaluation basis for governed decisions" evidence obligation
// (docs/architecture/03-microservices.md §8.1) that Evaluate's response
// alone does not: returning rule_basis/policy_version_id in an HTTP
// response is not durable evidence, only a record actually written to
// governance-decision-log-svc is.
package decisionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// globalScopeSentinel is substituted for TenantID/LegalEntityID when a
// policy evaluation had no tenant/entity scope (a global policy).
// governance-decision-log-svc requires both fields non-empty on every
// decision; policy-svc legitimately supports nil scope for global
// policies (see internal/domain "TenantID nil means ... applies
// globally"). This sentinel bridges that contract mismatch without
// requiring a schema or validation change on the decision-log side —
// confirmed against a real governance-decision-log-svc instance that
// this value is accepted with no special-casing needed.
const globalScopeSentinel = "GLOBAL"

// RecordDecisionParams holds the fields forwarded to
// governance-decision-log-svc's POST /v1/decisions.
type RecordDecisionParams struct {
	DecisionID        string
	TenantID          *string
	LegalEntityID     *string
	ActorID           string
	ActionType        string
	Outcome           string
	RuleBasis         string
	EvaluationContext json.RawMessage
	CorrelationID     string
}

// Client is the narrow interface Evaluate depends on for recording
// decisions. Allows the handler to be tested without a real HTTP call.
type Client interface {
	RecordDecision(ctx context.Context, params RecordDecisionParams) error
}

// HTTPClient implements Client against a real governance-decision-log-svc
// instance.
//
// Called synchronously from within Evaluate's request handling, matching
// this codebase's existing convention for calling out on the write path
// (identity-context-svc's and tenant-entity-registry-svc's real Kafka
// producers are also invoked synchronously, not fire-and-forget in a
// goroutine — see their internal/events/publisher.go). Failures are
// logged, not surfaced: Evaluate must not fail or block indefinitely
// because the evidence store is unavailable (Policy Service is Tier 0,
// "high execution criticality" per §8.1's Scaling Characteristics, and
// §3.9 requires governance enforcement not become a latency bottleneck).
//
// Timeout is deliberately tight (2s), not the more common 5s default —
// verified live against a fully-down governance-decision-log-svc that a
// DNS resolution failure alone took ~2.5s wall-clock with an unbounded
// client, which would otherwise silently become Evaluate's worst-case
// latency floor whenever the evidence store is unreachable. Revisit if
// this call proves to be an actual bottleneck in practice under normal
// (not fully-down) conditions — moving to async/best-effort delivery
// would be the next step, not a redesign.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://governance-decision-log-svc:8083" (no trailing slash).
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

// createDecisionRequest mirrors governance-decision-log-svc's own
// createDecisionRequest wire shape exactly (internal/handler/handler.go).
type createDecisionRequest struct {
	DecisionID        string          `json:"decision_id"`
	TenantID          string          `json:"tenant_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	ActorID           string          `json:"actor_id"`
	ActionType        string          `json:"action_type"`
	Outcome           string          `json:"outcome"`
	RuleBasis         string          `json:"rule_basis"`
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID     string          `json:"correlation_id"`
}

// RecordDecision POSTs to governance-decision-log-svc's /v1/decisions.
// Idempotent on DecisionID (that service's own idempotency key) — a
// caller-supplied DecisionID makes retries safe; an omitted one (Evaluate
// generates a fresh UUID per call) means a client-side retry could record
// a duplicate decision. That is a known, accepted limitation: Evaluate
// itself remains idempotent (pure read/compute), only the best-effort
// evidence side-channel is not, and only when the caller doesn't supply
// its own decision_id.
func (c *HTTPClient) RecordDecision(ctx context.Context, params RecordDecisionParams) error {
	tenantID := globalScopeSentinel
	if params.TenantID != nil && *params.TenantID != "" {
		tenantID = *params.TenantID
	}
	legalEntityID := globalScopeSentinel
	if params.LegalEntityID != nil && *params.LegalEntityID != "" {
		legalEntityID = *params.LegalEntityID
	}

	body, err := json.Marshal(createDecisionRequest{
		DecisionID:        params.DecisionID,
		TenantID:          tenantID,
		LegalEntityID:     legalEntityID,
		ActorID:           params.ActorID,
		ActionType:        params.ActionType,
		Outcome:           params.Outcome,
		RuleBasis:         params.RuleBasis,
		EvaluationContext: params.EvaluationContext,
		CorrelationID:     params.CorrelationID,
	})
	if err != nil {
		return fmt.Errorf("marshal decision request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build decision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if params.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", params.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call governance-decision-log-svc: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("governance-decision-log-svc returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
