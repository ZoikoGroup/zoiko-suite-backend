// Package authz provides the Authorization Service client interface and stub.
//
// Per doctrine: no domain service self-authorizes a material action.
// Every mutating API call in tenant-entity-registry-svc must receive an
// authorization decision from the Authorization Service before proceeding.
//
// This package defines the interface and a stub HTTP client. When the
// Authorization Service ships, `StubAuthZClient` is replaced by
// `HTTPAuthZClient` with a real gRPC or HTTP call — no other code changes.
package authz

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// AuthorizationClient is the contract for authorizing mutations.
// Callers pass the raw IdentityContextEnvelope JWT from the request's
// Authorization header; the AuthZ Service verifies it and evaluates
// the RBAC/ABAC decision for the requested resource and action.
type AuthorizationClient interface {
	// Authorize returns nil if the action is permitted.
	// Returns ErrUnauthorized if denied; ErrUnavailable if service unreachable.
	Authorize(ctx context.Context, envelopeJWT, resource, action string) error
}

// Sentinel errors — mapped to HTTP status codes in handlers.
var (
	// ErrUnauthorized is returned when the Authorization Service denies the action.
	ErrUnauthorized = fmt.Errorf("authorization denied")
	// ErrAuthZUnavailable is returned when the Authorization Service cannot be reached.
	// Callers must fail-closed (return 503) — no mutation proceeds without an authz decision.
	ErrAuthZUnavailable = fmt.Errorf("authorization service unavailable")
)

// StubAuthZClient is the development stub.
// TODO: replace with HTTPAuthZClient before Phase 1 production cutover.
// Every mutation will permit by default during development only.
type StubAuthZClient struct {
	log *zap.Logger
}

func NewStubAuthZClient(log *zap.Logger) *StubAuthZClient {
	return &StubAuthZClient{log: log}
}

func (c *StubAuthZClient) Authorize(ctx context.Context, envelopeJWT, resource, action string) error {
	c.log.Debug("authz stub — permitted (wire real AuthZ before production)",
		zap.String("resource", resource),
		zap.String("action", action),
	)
	return nil
}

// HTTPAuthZClient is the production implementation.
// TODO: implement against Authorization Service gRPC/HTTP contract.
type HTTPAuthZClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPAuthZClient(baseURL string, log *zap.Logger) *HTTPAuthZClient {
	return &HTTPAuthZClient{
		baseURL: baseURL,
		log:     log,
		client: &http.Client{
			Timeout: 2 * time.Second, // strict timeout — authz must not block the hot path
		},
	}
}

func (c *HTTPAuthZClient) Authorize(ctx context.Context, envelopeJWT, resource, action string) error {
	// TODO: POST {baseURL}/v1/authorize
	// Body: { "envelope_jwt": envelopeJWT, "resource": resource, "action": action }
	// 200 → permitted; 403 → ErrUnauthorized; network err → ErrAuthZUnavailable
	c.log.Warn("HTTPAuthZClient not yet implemented — falling through (dev mode only)",
		zap.String("resource", resource),
		zap.String("action", action),
	)
	return nil
}
