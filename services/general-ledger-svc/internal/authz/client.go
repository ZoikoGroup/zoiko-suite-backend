// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to perform a given ledger action.
//
// Doctrine (03-microservices.md): no service self-authorizes a material
// action. Every mutating journal action is checked against authorization-svc
// synchronously, fail-closed — an unreachable authorization-svc rejects the
// action, it never silently permits it. This is financial ledger data, so
// this client is wired for real from day one (unlike some Phase 1 services
// that shipped with a documented stub-first posture).
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

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil if principalID is authorized to perform
	// actionType within legalEntityID. Returns domain.ErrAuthorizationDenied
	// if authorization-svc says DENIED, or
	// domain.ErrAuthorizationServiceUnavailable if it cannot be reached —
	// callers must fail-closed on the latter.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
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
		// Tight timeout — a journal action must not stall indefinitely
		// because authorization-svc is slow.
		http: &http.Client{Timeout: 2 * time.Second, Transport: newRetryTransport()},
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

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(authorizeRequest{PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: actionType})
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
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID), zap.String("action_type", actionType), zap.Error(err))
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
