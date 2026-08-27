// Package authz provides the Authorization Service client.
//
// Per doctrine: no domain service self-authorizes a material action.
// Every mutating API call in tenant-entity-registry-svc must receive an
// authorization decision from authorization-svc before proceeding.
//
// This client FAILS CLOSED. An unreachable, slow, or unreadable
// authorization-svc rejects the mutation; it never silently permits it.
//
// Until 2026-08-05 HTTPAuthZClient.Authorize was a TODO that logged a warning
// and returned nil, and docker-compose pointed AUTHZ_SERVICE_URL at this
// service itself ("mock stub authz points back"). Because main.go treated any
// URL other than the literal "http://authorization-svc" as production wiring,
// that combination selected the no-op client and logged "using HTTP
// authorization client" while permitting every mutation without a decision.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/tenant-entity-registry-svc/internal/envelope"
)

// AuthorizationClient is the contract for authorizing mutations.
//
// principalID is the acting principal, resolved from the gateway-verified
// X-Principal-Id header — never from a token this service parses itself.
// scopeID is the authorization scope the decision is evaluated in: the legal
// entity for entity-level actions, or the tenant for tenant-level ones.
// authorization-svc rejects an empty legal_entity_id with 400.
type AuthorizationClient interface {
	// Authorize returns nil if the action is permitted.
	// Returns ErrUnauthorized if denied; ErrAuthZUnavailable if the service is
	// unreachable or answers in a way that cannot be read as a decision.
	Authorize(ctx context.Context, principalID, scopeID, resource, action string) error
}

// Sentinel errors — mapped to HTTP status codes in handlers.
var (
	// ErrUnauthorized is returned when authorization-svc denies the action (403).
	ErrUnauthorized = fmt.Errorf("authorization denied")
	// ErrAuthZUnavailable is returned when authorization-svc cannot be reached (503).
	// Callers must fail-closed — no mutation proceeds without a decision.
	ErrAuthZUnavailable = fmt.Errorf("authorization service unavailable")
)

// StubAuthZClient is the development/CI stub. Permits everything.
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
			Timeout: authzTimeout(),
		},
	}
}

// authorizeRequest matches authorization-svc's POST /v1/authorize body
// (services/authorization-svc/internal/handler/handler.go). All three fields
// are required; an empty one is answered 400, which this client treats as
// unavailable (fail-closed), not as a denial.
type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse matches authorization-svc's response. Both GRANTED and
// DENIED come back as HTTP 200 — the status reflects "the evaluation
// succeeded", not the outcome — so the body must always be read.
type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

// ActionType renders a resource/action pair as the flat action_type vocabulary
// authorization-svc stores in permission bundles, e.g.
// ("entity.hierarchy", "create") → "ENTITY_HIERARCHY_CREATE".
func ActionType(resource, action string) string {
	s := resource + "_" + action
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return strings.ToUpper(s)
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

	// Forward the caller's canonical envelope. Without this the outbound request
	// carried Content-Type and nothing else, authorization-svc refused it with 401
	// envelope_incomplete, and this client turned that into "authorization service
	// unavailable" -- failing closed on every authorized write while
	// authorization-svc was healthy and answering correctly.
	svcenvelope.ForwardTo(ctx, req)

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

// devPlaceholderURLs are AUTHZ_SERVICE_URL values that mean "nobody has wired
// this yet". "http://tenant-svc:8081" is in the list because docker-compose
// pointed this service's authz URL back at itself as a mock — harmless while
// Authorize was a no-op, a self-call 404 (and so a fail-closed 503 on every
// mutation) now that it is real.
var devPlaceholderURLs = map[string]bool{
	"":                         true,
	"http://authorization-svc": true,
	"http://tenant-svc:8081":   true,
}

// NewClient constructs an AuthorizationClient based on environment and config.
// In production or staging a placeholder or empty baseURL is a fatal
// misconfiguration rather than a silent fallback to StubAuthZClient.
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

// authzTimeout is the bound on one call to authorization-svc.
//
// TWO SECONDS WAS RIGHT AND IS NOT ANY MORE. It was chosen when every service
// and the database sat on one Docker network, where an authorize call is a
// sub-millisecond hop. authorization-svc now writes an access_decision_log row
// to a managed Postgres before it answers -- doctrine requires the artifact
// before the caller gets a decision -- so the call costs a real round trip to
// wherever that database lives. Measured at ~1.6s against a Supabase pooler on
// another continent, which fits inside 2s until it does not: the failure is
// "context canceled" at exactly 2.000s, surfaced as authz_unavailable, and the
// write is refused for a reason that has nothing to do with authorization.
//
// Kept as an environment knob with the original default, so nothing changes for
// a co-located deployment and a high-latency one can say so.
func authzTimeout() time.Duration {
	if raw := os.Getenv("AUTHZ_HTTP_TIMEOUT_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 2 * time.Second
}
