// Package decisionlog delivers workflow decisions to
// governance-decision-log-svc's POST /v1/decisions (GOV §1 "no evidence
// afterthought"). The record is written to outbox_events in the decision's own
// transaction; LedgerDeliverer is the relay's sender. The request-path
// client it replaces sent a body the ledger refused (Go-cased keys, no
// decision_id or rule_basis) and only logged the failure (row 39).
package decisionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/outbox"
)

// LedgerPurposeContext is sent as X-Purpose-Context on every ledger append.
const LedgerPurposeContext = "workflow_decision_evidence"

// LedgerDeliverer is the outbox deliverer for destination decision_log: it
// appends one workflow decision's LedgerRecord to governance-decision-log-svc.
//
// Before 21 Oct 2026 the handler called RecordDecision after the decision had
// committed, on the request path, and a failure only logged a warning: an
// approval could stand with no ledger record, and nothing ever retried it
// (Governance audit, workflow row 31). The record is now written to the outbox
// in the decision's own transaction and this deliverer sends it with backoff
// until the ledger has it.
//
// The writer is workflow-svc's own service principal (as policy-svc's, D-02):
// only it needs GOVERNANCE_DECISION_RECORD. The person who decided travels in
// evaluation_context.actor_principal_id.
type LedgerDeliverer struct {
	baseURL     string
	principalID string
	http        *http.Client
}

// NewLedgerDeliverer binds the deliverer to governance-decision-log-svc at
// baseURL, writing as principalID.
func NewLedgerDeliverer(baseURL, principalID string) *LedgerDeliverer {
	return &LedgerDeliverer{
		baseURL:     strings.TrimRight(baseURL, "/"),
		principalID: principalID,
		http:        &http.Client{Timeout: 5 * time.Second},
	}
}

type ledgerRequest struct {
	DecisionID          string          `json:"decision_id"`
	LegalEntityID       string          `json:"legal_entity_id"`
	ActionType          string          `json:"action_type"`
	Outcome             string          `json:"outcome"`
	RuleBasis           string          `json:"rule_basis"`
	JurisdictionContext *string         `json:"jurisdiction_context,omitempty"`
	ActionSubjectType   *string         `json:"action_subject_type,omitempty"`
	ActionSubjectID     *string         `json:"action_subject_id,omitempty"`
	EvaluationContext   json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID       string          `json:"correlation_id"`
	WorkflowInstanceID  *string         `json:"workflow_instance_id,omitempty"`
	CausationID         *string         `json:"causation_id,omitempty"`
	DecidedAt           time.Time       `json:"decided_at"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// clip keeps a value inside the ledger's column limit; the full value stays in
// evaluation_context.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Deliver implements outbox.Deliverer.
func (d *LedgerDeliverer) Deliver(ctx context.Context, e outbox.StoredEvent) error {
	if d.principalID == "" {
		return fmt.Errorf("WORKFLOW_LEDGER_PRINCIPAL_ID is not set; the record stays pending")
	}
	var rec domain.LedgerRecord
	if err := json.Unmarshal(e.Payload, &rec); err != nil {
		return fmt.Errorf("%w: undecodable ledger record: %v", outbox.ErrPermanent, err)
	}
	evaluation, err := json.Marshal(rec.EvaluationContext)
	if err != nil {
		return fmt.Errorf("%w: evaluation context: %v", outbox.ErrPermanent, err)
	}
	correlation := rec.CorrelationID
	if correlation == "" {
		// The ledger requires one; the instance is the flow this decision
		// belongs to.
		correlation = rec.WorkflowInstanceID
	}
	body, err := json.Marshal(ledgerRequest{
		DecisionID:          rec.DecisionID,
		LegalEntityID:       rec.LegalEntityID,
		ActionType:          rec.ActionType,
		Outcome:             rec.Outcome,
		RuleBasis:           clip(rec.RuleBasis, 256),
		JurisdictionContext: optional(rec.JurisdictionContext),
		ActionSubjectType:   optional(clip(rec.ActionSubjectType, 64)),
		ActionSubjectID:     optional(clip(rec.ActionSubjectID, 64)),
		EvaluationContext:   evaluation,
		CorrelationID:       clip(correlation, 64),
		WorkflowInstanceID:  optional(rec.WorkflowInstanceID),
		CausationID:         optional(rec.CausationID),
		DecidedAt:           rec.DecidedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", outbox.ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build ledger request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", rec.TenantID)
	h.Set("X-Principal-Id", d.principalID)
	h.Set("X-Legal-Entity-Id", rec.LegalEntityID)
	h.Set("X-Correlation-ID", clip(correlation, 64))
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", LedgerPurposeContext)
	// Deterministic: a redelivery after a lost response is a replay (200).
	h.Set("Idempotency-Key", "workflow-ledger:"+rec.DecisionID)
	if rec.CausationID != "" {
		h.Set("X-Causation-Id", rec.CausationID)
	}
	if rec.JurisdictionContext != "" {
		h.Set("X-Jurisdiction-Context", rec.JurisdictionContext)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("call governance-decision-log-svc: %w", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusConflict:
		// A different decision already holds this id or key: resending the
		// same record cannot succeed.
		return fmt.Errorf("%w: %d: %s", outbox.ErrPermanent, resp.StatusCode, msg)
	default:
		// 400 (a contract mismatch between versions), 401/403 (the writer's
		// grant is missing), 5xx: a deploy, a grant or time fixes these.
		return fmt.Errorf("governance-decision-log-svc returned %d: %s", resp.StatusCode, msg)
	}
}
