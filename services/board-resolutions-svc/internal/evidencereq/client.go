// Package evidencereq verifies that the evidence required before a
// resolution may be passed actually exists, per evidence-requirements-svc's
// catalog (03-microservices.md §8.6: "No finalization path may skip
// required evidence states"). Closes a gap where PassResolution accepted a
// document_vault_id from the caller but never checked whether any evidence
// was actually required, or whether what was offered satisfied it.
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
	svcenvelope "zoiko.io/board-resolutions-svc/internal/envelope"
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

	// evidence-requirements-svc validates the same canonical envelope contract
	// this service does and answers 401 envelope_incomplete without it. A
	// non-200 is treated as unavailable below, so an unforwarded envelope
	// turned every pass attempt into a misleading "unavailable" refusal
	// instead of the fixable validation error it actually was. Same defect,
	// same fix as internal/authz.Client.checkAllowedLive.
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
	req.Header.Set("X-Request-Id", evidenceRequestID)
	req.Header.Set("X-Source-Channel", evidenceSourceChannel)
	// One evaluation per (request, correlation): correlationID already
	// distinguishes a genuine retry from a distinct attempt (see the caller's
	// comment on why it is never defaulted to the resolution's own ID), so it
	// doubles as the idempotency key here rather than minting a third value.
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

	// Allow-list, not a deny-list.
	//
	// This was `if res.Outcome == "MISSING" { return ErrEvidenceMissing };
	// return nil` — everything that was not the single word MISSING passed,
	// which contradicted the doc comment two lines above it and, worse, failed
	// OPEN. An empty outcome (the zero value, which is what a response shape
	// that changed or a field renamed on the other side produces), a typo, or
	// a new outcome the catalog starts returning would all have let a
	// resolution be passed with its evidence gate unchecked — silently, since
	// nothing distinguishes "SATISFIED" from "the field wasn't there" once the
	// comparison is against one specific other value.
	switch res.Outcome {
	case "SATISFIED", "NO_REQUIREMENTS_DEFINED":
		return nil
	case "MISSING":
		return ErrEvidenceMissing
	default:
		return fmt.Errorf("%w: unrecognised evaluation outcome %q", ErrServiceUnavailable, res.Outcome)
	}
}
