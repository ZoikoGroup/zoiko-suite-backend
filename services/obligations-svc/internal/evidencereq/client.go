// Package evidencereq provides a client for evidence-requirements-svc.
package evidencereq

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
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// Client is the interface for calling evidence-requirements-svc.
type Client interface {
	Evaluate(ctx context.Context, params EvaluateParams) (*EvaluateResponse, error)
}

type EvaluateParams struct {
	LegalEntityID    string
	DomainCode       string
	ActionType       string
	CorrelationID    string
	PresentArtifacts []Artifact
}

type Artifact struct {
	EvidenceType    string `json:"evidence_type"`
	ReferenceID     string `json:"reference_id"`
	ArtifactSubtype string `json:"artifact_subtype,omitempty"`
}

type EvaluateResponse struct {
	EvaluationID  string    `json:"evaluation_id"`
	Outcome       string    `json:"outcome"`
	Unmet         []Unmet   `json:"unmet"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	CorrelationID string    `json:"correlation_id"`
	DomainCode    string    `json:"domain_code"`
	ActionType    string    `json:"action_type"`
}

type Unmet struct {
	EvidenceRequirementID string `json:"evidence_requirement_id"`
	EvidenceType          string `json:"evidence_type"`
	Reason                string `json:"reason"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
		log:     log,
	}
}

type evaluateRequest struct {
	LegalEntityID    string     `json:"legal_entity_id"`
	DomainCode       string     `json:"domain_code"`
	ActionType       string     `json:"action_type"`
	CorrelationID    string     `json:"correlation_id"`
	PresentArtifacts []Artifact `json:"present_artifacts,omitempty"`
}

func (c *HTTPClient) Evaluate(ctx context.Context, params EvaluateParams) (*EvaluateResponse, error) {
	reqBody := evaluateRequest{
		LegalEntityID:    params.LegalEntityID,
		DomainCode:       params.DomainCode,
		ActionType:       params.ActionType,
		CorrelationID:    params.CorrelationID,
		PresentArtifacts: params.PresentArtifacts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/evidence/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrEvidenceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", params.CorrelationID)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	// The caller's identity travels with the evaluation: the signed envelope
	// and the canonical headers (ER-S7 / OB-S16).
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)

	// Forward the caller's canonical envelope for downstream traceability
	// (handled by the calling service's envelope middleware)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("evidence-requirements-svc unreachable",
			zap.String("legal_entity_id", params.LegalEntityID),
			zap.String("action_type", params.ActionType),
			zap.Error(err))
		return nil, domain.ErrEvidenceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("evidence-requirements-svc returned error",
			zap.String("legal_entity_id", params.LegalEntityID),
			zap.String("action_type", params.ActionType),
			zap.Int("status_code", resp.StatusCode))
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			return nil, domain.ErrAuthorizationDenied
		}
		return nil, fmt.Errorf("%w: evidence evaluation answered %d", domain.ErrEvidenceUnavailable, resp.StatusCode)
	}

	var result EvaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: undecodable evaluation: %v", domain.ErrEvidenceUnavailable, err)
	}

	return &result, nil
}

// Unconfigured answers every evaluation with domain.ErrEvidenceUnavailable:
// with no evidence-requirements-svc configured, nothing that needs evidence
// can pass (fail closed; rule R14 forbids an invented SATISFIED).
type Unconfigured struct{}

// Evaluate implements Client.
func (Unconfigured) Evaluate(context.Context, EvaluateParams) (*EvaluateResponse, error) {
	return nil, fmt.Errorf("%w: EVIDENCE_REQUIREMENTS_URL is not configured", domain.ErrEvidenceUnavailable)
}

var ErrEvidenceInsufficient = errors.New("evidence insufficient for obligation closure")
var ErrEvidenceEvaluationFailed = errors.New("evidence evaluation failed")
