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

	"go.uber.org/zap"
)

// ErrDecisionLogUnavailable is returned on any network error or non-200 response.
var ErrDecisionLogUnavailable = errors.New("governance-decision-log-svc unavailable")

// Client writes decision records to governance-decision-log-svc.
type Client interface {
	// RecordDecision writes a governance decision record.
	RecordDecision(ctx context.Context, params RecordDecisionParams) (decisionID string, err error)
}

type RecordDecisionParams struct {
	TenantID            string
	LegalEntityID       string
	ActorPrincipalID    string
	ActionType          string      // e.g. "WORKFLOW_APPROVED", "WORKFLOW_REJECTED", "WORKFLOW_ESCALATED", "WORKFLOW_CANCELLED", "WORKFLOW_INVALIDATED"
	ResourceType        string      // "workflow_instance"
	ResourceID          string
	Outcome             string      // "APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "INVALIDATED"
	Rationale           *string
	CorrelationID       string
	CausationID         *string
	WorkflowType        string
	WorkflowDefinitionID *string
	WorkflowDefinitionVersion *int
	EvidenceRefs        []string
	SubjectType         *string
	SubjectID           *string
	SubjectVersion      *int
	SubjectFingerprint  *string
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

func (c *HTTPClient) RecordDecision(ctx context.Context, params RecordDecisionParams) (string, error) {
	body, err := json.Marshal(params)
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
	if params.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", params.CorrelationID)
	}
	if params.CausationID != nil {
		req.Header.Set("X-Causation-Id", *params.CausationID)
	}

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