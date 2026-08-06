// Package authz provides the Authorization Service client interface and implementations.
//
// Per doctrine: no domain service self-authorizes a material action.
// Every mutating API call in jurisdiction-rules-svc must receive an
// authorization decision from the Authorization Service before proceeding.
//
// The HTTP client FAILS CLOSED. An unreachable, slow, or misbehaving
// authorization-svc rejects the mutation; it never silently permits it.
// Until 2026-08-05 HTTPAuthZClient.Authorize was a TODO that returned nil
// unconditionally — so any deployment with a non-placeholder
// AUTHZ_SERVICE_URL (which is exactly what the production-startup guard
// below forces) permitted every admin mutation without a decision. Same
// posture as bank-reconciliation-svc and evidence-requirements-svc.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AuthorizationClient is the contract for authorizing mutations.
//
// principalID is the acting principal resolved from the gateway-verified
// identity envelope. scopeID is the authorization scope the decision is
// evaluated in — jurisdiction data is platform-wide reference data with no
// tenant or legal entity of its own, so callers pass the configured
// platform scope (see Config.AuthZPlatformScopeID); authorization-svc
// rejects an empty legal_entity_id outright.
type AuthorizationClient interface {
	// Authorize returns nil if the action is permitted.
	// Returns ErrUnauthorized if denied; ErrAuthZUnavailable if the service
	// is unreachable or answers in a way that cannot be read as a decision.
	Authorize(ctx context.Context, principalID, scopeID, resource, action string) error
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
// NewClient refuses to construct it in production or staging.
type StubAuthZClient struct {
	log *zap.Logger
}

func NewStubAuthZClient(log *zap.Logger) *StubAuthZClient {
	return &StubAuthZClient{log: log}
}

func (c *StubAuthZClient) Authorize(_ context.Context, principalID, _, resource, action string) error {
	c.log.Debug("authz stub — permitted (wire real AuthZ before production)",
		zap.String("principal_id", principalID),
		zap.String("resource", resource),
		zap.String("action", action),
	)
	return nil
}

// HTTPAuthZClient is the production implementation against authorization-svc.
type HTTPAuthZClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPAuthZClient(baseURL string, log *zap.Logger) *HTTPAuthZClient {
	return &HTTPAuthZClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		client: &http.Client{
			Timeout: 2 * time.Second, // strict timeout — authz must not block the hot path
		},
	}
}

// authorizeRequest matches authorization-svc's POST /v1/authorize body
// (services/authorization-svc/internal/handler/handler.go). All three
// fields are required — an empty one is answered with 400, which this
// client treats as unavailable (fail-closed), not as a denial.
type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse matches authorization-svc's response. Both GRANTED and
// DENIED come back as HTTP 200 — the status reflects "the evaluation
// succeeded", not the outcome, so the body must always be read.
type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

// ActionType renders a resource/action pair as the flat action_type vocabulary
// authorization-svc stores in permission bundles, e.g.
// ("jurisdiction_rule", "transition") → "JURISDICTION_RULE_TRANSITION".
func ActionType(resource, action string) string {
	return strings.ToUpper(resource + "_" + action)
}

func (c *HTTPAuthZClient) Authorize(ctx context.Context, principalID, scopeID, resource, action string) error {
	actionType := ActionType(resource, action)

	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: scopeID,
		ActionType:    actionType,
	})
	if err != nil {
		return fmt.Errorf("marshal authorize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return ErrAuthZUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		return ErrAuthZUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", actionType),
			zap.ByteString("body", respBody),
		)
		return ErrAuthZUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.log.Error("unreadable decision body from authorization-svc — failing closed",
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		return ErrAuthZUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		c.log.Info("authorization denied",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.String("decision_basis", out.DecisionBasis),
			zap.String("access_decision_id", out.AccessDecisionID),
		)
		return ErrUnauthorized
	}
	return nil
}

// devPlaceholderURLs are the AUTHZ_SERVICE_URL values that mean "nobody has
// wired this yet". "http://jurisdiction-svc:8082" is in the list because
// docker-compose.yml used to point this service's authz URL back at itself
// as a mock — harmless while Authorize was a no-op, a self-call 404 (and so
// a fail-closed 503 on every mutation) now that it is real.
var devPlaceholderURLs = map[string]bool{
	"":                             true,
	"http://authorization-svc":     true,
	"http://jurisdiction-svc:8082": true,
}

// NewClient constructs an AuthorizationClient based on environment and config.
// Production-startup guard: in production or staging (ENV=production|staging)
// a placeholder or empty baseURL is a fatal misconfiguration rather than a
// silent fallback to StubAuthZClient.
func NewClient(env string, baseURL string, log *zap.Logger) (AuthorizationClient, error) {
	isProdOrStaging := strings.EqualFold(env, "production") || strings.EqualFold(env, "staging")
	isPlaceholder := devPlaceholderURLs[strings.TrimRight(baseURL, "/")]

	if isProdOrStaging && isPlaceholder {
		return nil, fmt.Errorf("security violation: cannot use StubAuthZClient or placeholder AuthZServiceURL (%q) in %s environment", baseURL, env)
	}

	if !isPlaceholder {
		log.Info("using HTTP authorization client", zap.String("url", baseURL))
		return NewHTTPAuthZClient(baseURL, log), nil
	}

	log.Warn("using STUB authorization client — wire real AuthZ before production",
		zap.String("authz_service_url", baseURL),
	)
	return NewStubAuthZClient(log), nil
}
