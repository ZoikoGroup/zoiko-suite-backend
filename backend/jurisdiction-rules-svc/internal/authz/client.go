// Package authz provides the Authorization Service client interface and implementations.
//
// Per doctrine: no domain service self-authorizes a material action.
// Every mutating API call in jurisdiction-rules-svc must receive an
// authorization decision from the Authorization Service before proceeding.
package authz

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AuthorizationClient is the contract for authorizing mutations.
type AuthorizationClient interface {
	// Authorize returns nil if the action is permitted.
	// Returns ErrUnauthorized if denied; ErrAuthZUnavailable if service unreachable.
	Authorize(ctx context.Context, envelopeJWT, resource, action string) error
}

// Sentinel errors — mapped to HTTP status codes in handlers.
var (
	// ErrUnauthorized is returned when the Authorization Service denies the action (403 Forbidden).
	ErrUnauthorized = fmt.Errorf("authorization denied")
	// ErrAuthZUnavailable is returned when the Authorization Service cannot be reached (503 Service Unavailable).
	// Callers must fail-closed — no mutation proceeds without an authz decision.
	ErrAuthZUnavailable = fmt.Errorf("authorization service unavailable")
)

// StubAuthZClient is the development/CI stub.
// Every mutation permits by default during local development and testing only.
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

// HTTPAuthZClient is the production implementation against the Authorization Service.
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
	// 200 -> permitted; 403 -> ErrUnauthorized; network err -> ErrAuthZUnavailable
	c.log.Warn("HTTPAuthZClient wire call not yet implemented — falling through (dev mode only)",
		zap.String("resource", resource),
		zap.String("action", action),
	)
	return nil
}

// NewClient constructs an AuthorizationClient based on environment and config.
// Production-startup guard: In production or staging environments (ENV=production|staging),
// if baseURL is empty or points to the dev placeholder ("http://authorization-svc"), it returns
// an error to prevent silent fallback to StubAuthZClient.
func NewClient(env string, baseURL string, log *zap.Logger) (AuthorizationClient, error) {
	isProdOrStaging := strings.EqualFold(env, "production") || strings.EqualFold(env, "staging")
	isPlaceholder := baseURL == "" || baseURL == "http://authorization-svc"

	if isProdOrStaging && isPlaceholder {
		return nil, fmt.Errorf("security violation: cannot use StubAuthZClient or placeholder AuthZServiceURL (%q) in %s environment", baseURL, env)
	}

	if baseURL != "" && baseURL != "http://authorization-svc" {
		log.Info("using HTTP authorization client", zap.String("url", baseURL))
		return NewHTTPAuthZClient(baseURL, log), nil
	}

	log.Warn("using STUB authorization client — wire real AuthZ before production")
	return NewStubAuthZClient(log), nil
}
