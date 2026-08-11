// Package evidencereq verifies that the evidence required before a filing
// draft may be validated as PREPARED actually exists, per
// evidence-requirements-svc's catalog (03-microservices.md §8.6: "No
// finalization path may skip required evidence states").
//
// Replaces a gap where FilingDraft.ValidateEvidence only checked whether a
// caller-supplied list of required document types was non-empty — a check
// the caller itself controlled, so a client could always pass validation by
// sending an empty list regardless of the platform's actual evidence
// catalog for this filing type.
package evidencereq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors. Callers must fail closed: any error returned means the
// action must be refused, same doctrine as the authz.Client in this service.
var (
	// ErrEvidenceMissing means the catalog has an effective requirement that
	// the presented artifacts did not satisfy.
	ErrEvidenceMissing = errors.New("required evidence is missing")
	// ErrServiceUnavailable means evidence-requirements-svc could not be
	// reached, timed out, or returned an unexpected response.
	ErrServiceUnavailable = errors.New("evidence-requirements-svc unavailable")
)

// Artifact is one piece of evidence the caller asserts exists.
type Artifact struct {
	EvidenceType    string `json:"evidence_type"`
	ReferenceID     string `json:"reference_id"`
	ArtifactSubtype string `json:"artifact_subtype,omitempty"`
}

// Client verifies evidence sufficiency against evidence-requirements-svc.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new evidence-requirements verification client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

type evaluateRequest struct {
	LegalEntityID    string     `json:"legal_entity_id"`
	DomainCode       string     `json:"domain_code"`
	ActionType       string     `json:"action_type"`
	PresentArtifacts []Artifact `json:"present_artifacts"`
	CorrelationID    string     `json:"correlation_id"`
}

type unmetRequirement struct {
	EvidenceType string `json:"evidence_type"`
	Reason       string `json:"reason"`
}

type evaluateResponse struct {
	Outcome string             `json:"outcome"`
	Unmet   []unmetRequirement `json:"unmet"`
}

// EvaluateResult carries the outcome plus a human-readable reason string
// suitable for FilingDraft.BlockReasons — the platform's own recorded
// reason, not a caller's guess.
type EvaluateResult struct {
	Sufficient bool
	Reason     string
}

// Evaluate calls POST /v1/evidence/evaluate scoped to tenantID. Sufficient
// is true for SATISFIED or NO_REQUIREMENTS_DEFINED outcomes — i.e. there is
// nothing to withhold on. Any transport failure, non-200 response, or
// decode error returns ErrServiceUnavailable; callers must treat that as
// "refuse the action", not as Sufficient=false.
func (c *Client) Evaluate(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []Artifact) (EvaluateResult, error) {
	reqBody, err := json.Marshal(evaluateRequest{
		LegalEntityID:    legalEntityID,
		DomainCode:       domainCode,
		ActionType:       actionType,
		PresentArtifacts: artifacts,
		CorrelationID:    correlationID,
	})
	if err != nil {
		return EvaluateResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/evidence/evaluate", bytes.NewReader(reqBody))
	if err != nil {
		return EvaluateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return EvaluateResult{}, ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return EvaluateResult{}, ErrServiceUnavailable
	}

	var res evaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return EvaluateResult{}, ErrServiceUnavailable
	}

	if res.Outcome == "MISSING" {
		reason := "Required evidence is missing."
		if len(res.Unmet) > 0 {
			reason = res.Unmet[0].Reason
		}
		return EvaluateResult{Sufficient: false, Reason: reason}, nil
	}
	return EvaluateResult{Sufficient: true}, nil
}
