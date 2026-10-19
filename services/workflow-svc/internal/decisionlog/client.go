// Package decisionlog writes governance decisions to the decision-log-svc
// per GOV §1 "No evidence afterthought" and §2 invariant #9.
package decisionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
)

// ErrDecisionLogUnavailable is returned on any network error or non-200 response.
var ErrDecisionLogUnavailable = errors.New("governance-decision-log-svc unavailable")

// Client writes decision records to governance-decision-log-svc.
type Client interface {
	// RecordDecision writes a governance decision record.
	RecordDecision(ctx context.Context, params RecordDecisionParams) (decisionID string, err error)
}

type RecordDecisionParams struct {
	TenantID                  string
	LegalEntityID             string
	ActorPrincipalID          string
	ActionType                string // e.g. "WORKFLOW_APPROVED", "WORKFLOW_REJECTED", "WORKFLOW_ESCALATED", "WORKFLOW_CANCELLED", "WORKFLOW_INVALIDATED"
	ResourceType              string // "workflow_instance"
	ResourceID                string
	Outcome                   string // "APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "INVALIDATED"
	Rationale                 *string
	CorrelationID             string
	CausationID               *string
	WorkflowType              string
	WorkflowDefinitionID      *string
	WorkflowDefinitionVersion *int
	EvidenceRefs              []string
	SubjectType               *string
	SubjectID                 *string
	SubjectVersion            *int
	SubjectFingerprint        *string
}

type decisionLogResponse struct {
	DecisionID string `json:"decision_id"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

type decisionRequest struct {
	DecisionID         string         `json:"decision_id"`
	TenantID           string         `json:"tenant_id,omitempty"`
	LegalEntityID      string         `json:"legal_entity_id"`
	ActionType         string         `json:"action_type"`
	Outcome            string         `json:"outcome"`
	RuleBasis          string         `json:"rule_basis"`
	CorrelationID      string         `json:"correlation_id"`
	WorkflowInstanceID *string        `json:"workflow_instance_id,omitempty"`
	CausationID        *string        `json:"causation_id,omitempty"`
	EvaluationContext  map[string]any `json:"evaluation_context,omitempty"`
}

func (c *HTTPClient) RecordDecision(ctx context.Context, params RecordDecisionParams) (string, error) {
	correlationID := params.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	// Deterministic decision_id makes an at-least-once retry a replay of the
	// same decision instead of a second governance record.
	decisionKey := params.TenantID + "|" + correlationID + "|" + params.ActionType + "|" + params.ResourceID + "|" + params.ActorPrincipalID
	if params.CausationID != nil {
		decisionKey += "|" + *params.CausationID
	}
	decisionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(decisionKey)).String()

	ruleBasis := "workflow:" + params.ActionType
	if params.WorkflowDefinitionID != nil && *params.WorkflowDefinitionID != "" {
		ruleBasis += ":" + *params.WorkflowDefinitionID
		if params.WorkflowDefinitionVersion != nil {
			ruleBasis += fmt.Sprintf(":v%d", *params.WorkflowDefinitionVersion)
		}
	}

	var workflowInstanceID *string
	if params.ResourceID != "" {
		workflowInstanceID = &params.ResourceID
	}

	evaluationContext := map[string]any{
		"workflow_type": params.WorkflowType,
		"resource_type": params.ResourceType,
		"resource_id":   params.ResourceID,
	}
	if params.Rationale != nil {
		evaluationContext["rationale"] = *params.Rationale
	}
	if len(params.EvidenceRefs) > 0 {
		evaluationContext["evidence_refs"] = params.EvidenceRefs
	}

	body, err := json.Marshal(decisionRequest{
		DecisionID:         decisionID,
		TenantID:           params.TenantID,
		LegalEntityID:      params.LegalEntityID,
		ActionType:         params.ActionType,
		Outcome:            params.Outcome,
		RuleBasis:          ruleBasis,
		CorrelationID:      correlationID,
		WorkflowInstanceID: workflowInstanceID,
		CausationID:        params.CausationID,
		EvaluationContext:  evaluationContext,
	})
	if err != nil {
		return "", fmt.Errorf("marshal decision log request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return "", ErrDecisionLogUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", params.TenantID)
	req.Header.Set("X-Principal-Id", params.ActorPrincipalID)
	req.Header.Set("X-Legal-Entity-Id", params.LegalEntityID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "governance_decision")
	req.Header.Set("Idempotency-Key", "workflow-decision:"+decisionID)
	if params.CausationID != nil {
		req.Header.Set("X-Causation-Id", *params.CausationID)
	}

	// Forward the caller's canonical envelope for downstream traceability.
	// The explicit headers above are the GDL contract; ForwardTo fills any
	// that the caller already supplied.
	svcenvelope.ForwardTo(ctx, req)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("governance-decision-log-svc unreachable", zap.Error(err))
		return "", ErrDecisionLogUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		c.log.Error("governance-decision-log-svc refused decision record", zap.Int("status", resp.StatusCode))
		return "", ErrDecisionLogUnavailable
	}

	var out decisionLogResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", ErrDecisionLogUnavailable
	}
	return out.DecisionID, nil
}
