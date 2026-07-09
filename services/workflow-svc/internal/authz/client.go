// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to submit an approval action.
//
// Doctrine (03-microservices.md §8.4): "Approval workflows extend
// authorization. They do not replace it." Every approval action is
// checked against authorization-svc synchronously, fail-closed — an
// unreachable authorization-svc rejects the action, it never silently
// permits it.
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

	"zoiko.io/workflow-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckApprovalAllowed returns nil if principalID is authorized to
	// approve within legalEntityID. Returns domain.ErrAuthorizationDenied
	// if authorization-svc says DENIED, or
	// domain.ErrAuthorizationServiceUnavailable if it cannot be reached —
	// callers must fail-closed on the latter, same as every other
	// synchronous cross-service call in this platform.
	CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID string) error
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
		// Tight timeout — an approval submission must not stall
		// indefinitely because authorization-svc is slow.
		http: &http.Client{Timeout: 2 * time.Second},
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

// approvalActionType is the action_type checked against authorization-svc
// for every approval submission. A single, platform-wide action type is
// intentional for v1 — nothing in the docs specifies per-workflow-type
// action codes.
const approvalActionType = "WORKFLOW_APPROVE"

func (c *HTTPClient) CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID string) error {
	body, err := json.Marshal(authorizeRequest{PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: approvalActionType})
	if err != nil {
		return fmt.Errorf("marshal authorize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed", zap.String("principal_id", principalID), zap.Error(err))
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
		return domain.ErrAuthorizationDenied
	}
	return nil
}
