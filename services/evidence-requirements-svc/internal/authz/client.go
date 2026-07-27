// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to mutate this service's evidence
// requirement catalog.
//
// Doctrine (03-microservices.md §17.1): no service self-authorizes a
// material action. Being a Governance Plane service is not an exemption —
// changing what evidence an action requires is itself a material governance
// act, so it routes through authorization-svc like anything else.
//
// Two things this client does deliberately, both in reaction to live
// defects found in the 2026-07-23 platform audit:
//
//  1. It FAILS CLOSED. An unreachable or misbehaving authorization-svc
//     rejects the action; it never silently permits it. The authz clients in
//     all ten Phase 5 services and in offboarding-severance-svc /
//     workforce-compliance-svc fail *open* on network error.
//  2. It is actually CALLED. All ten Phase 5 services construct an authz
//     client, inject it into the handler, and never invoke it on any route —
//     the gate is dead code. Every mutating route in
//     internal/handler.RegisterRoutes calls CheckAllowed before touching the
//     store; internal/handler/handler_test.go asserts that.
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

	"zoiko.io/evidence-requirements-svc/internal/domain"
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
		// Tight timeout — a catalog mutation must not stall indefinitely
		// because authorization-svc is slow.
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
