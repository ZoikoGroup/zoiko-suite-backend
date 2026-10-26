// Package evidencereq verifies that the evidence required before a
// corporate action may be executed actually exists, per
// evidence-requirements-svc's catalog (03-microservices.md §8.6: "No
// finalization path may skip required evidence states"). Closes a gap
// where ExecuteAction accepted a document_vault_id from the caller but
// never checked whether any evidence was actually required, or whether
// what was offered satisfied it.
package evidencereq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	svcenvelope "zoiko.io/corporate-actions-svc/internal/envelope"
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

// EvaluateSufficient calls POST /v1/evidence/evaluate scoped to tenantID and
// returns nil only when the outcome is SATISFIED or NO_REQUIREMENTS_DEFINED
// — i.e. there is nothing to withhold on. Any transport failure, non-200
// response, decode error, or a MISSING outcome all result in a non-nil
// error; callers must treat all of these as "refuse the action."
func (c *Client) EvaluateSufficient(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []Artifact) error {
	reqBody, err := json.Marshal(evaluateRequest{
		LegalEntityID:    legalEntityID,
		DomainCode:       domainCode,
		ActionType:       actionType,
		PresentArtifacts: artifacts,
		CorrelationID:    correlationID,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/evidence/evaluate", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Correlation-ID", correlationID)

	// evidence-requirements-svc enforces the canonical input envelope and
	// answers 401/400 without it. Forward the same verified context this
	// service already forwards to authorization-svc, and use the business
	// correlation id as the evaluation's idempotency key.
	evidenceRequestID := middleware.GetReqID(ctx)
	evidenceSourceChannel := "system"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			evidenceRequestID = env.RequestID
		}
		if env.SourceChannel != "" {
			evidenceSourceChannel = string(env.SourceChannel)
		}
	}
	if evidenceRequestID == "" {
		evidenceRequestID = correlationID
	}
	req.Header.Set("X-Request-Id", evidenceRequestID)
	req.Header.Set("X-Source-Channel", evidenceSourceChannel)
	req.Header.Set("Idempotency-Key", correlationID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ErrServiceUnavailable
	}

	var res evaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrServiceUnavailable
	}

	// Allow-list, not a deny-list: an empty or unknown outcome must not pass.
	switch res.Outcome {
	case "SATISFIED", "NO_REQUIREMENTS_DEFINED":
		return nil
	case "MISSING":
		return ErrEvidenceMissing
	default:
		return fmt.Errorf("%w: unrecognised evaluation outcome %q", ErrServiceUnavailable, res.Outcome)
	}
}
