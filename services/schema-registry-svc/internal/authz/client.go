// Package authz confirms, via authorization-svc, that a principal is
// actually allowed to mutate an event contract (register a schema version).
//
// Doctrine (docs/architecture/05-security.md §14.6 "Schema Security
// Alignment"): schema-registry access and event-contract mutation rights
// must be protected — unauthorized schema publication is a named risk.
// Every registration is checked against authorization-svc synchronously and
// fail-closed: an unreachable authorization-svc rejects the mutation, it
// never silently permits it. This is the same posture workflow-svc uses for
// approval actions.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckSchemaPublishAllowed returns nil if principalID is authorized to
	// publish/mutate event schemas within legalEntityID. Returns
	// domain.ErrPublishDenied if authorization-svc says DENIED, or
	// domain.ErrAuthorizationServiceUnavailable if it cannot be reached —
	// callers must fail-closed on the latter.
	CheckSchemaPublishAllowed(ctx context.Context, principalID, legalEntityID, correlationID string) error
}

// HTTPClient implements Client against a real authorization-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://authorization-svc:8089" (no trailing slash).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// publishActionType is the action_type checked against authorization-svc for
// every schema registration. A single, platform-wide action type is
// intentional for v1 — nothing in the docs specifies per-event-name action
// codes.
const publishActionType = "SCHEMA_PUBLISH"

func (c *HTTPClient) CheckSchemaPublishAllowed(ctx context.Context, principalID, legalEntityID, correlationID string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    publishActionType,
	})
	if err != nil {
		return fmt.Errorf("marshal authorize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID), zap.Error(err))
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return domain.ErrAuthorizationServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return domain.ErrPublishDenied
	}
	return nil
}
