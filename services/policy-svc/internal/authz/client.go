// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to mutate this service's policy
// catalog.
//
// Doctrine (03-microservices.md §17.1): no service self-authorizes a
// material action. policy-svc previously had no gate at all — its config
// carried a comment stating that admin writes "do not call" the
// Authorization Service — so anyone who could reach the port could define a
// policy, publish a version, and activate it. Publishing an approval
// threshold is exactly the kind of governance act §17.1 exists to cover.
//
// This client FAILS CLOSED. An unreachable, slow, or misbehaving
// authorization-svc rejects the mutation; it never silently permits it.
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

	"zoiko.io/policy-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil if principalID is authorized to perform
	// actionType within legalEntityID. Returns domain.ErrAuthorizationDenied
	// on a DENIED decision, or domain.ErrAuthorizationServiceUnavailable if
	// no decision could be obtained — callers must fail closed on the latter.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types this service asks authorization-svc about. These must exist
// in a permission bundle assigned to the calling principal, against the same
// legal entity the policy is scoped to.
const (
	ActionPolicyCreate          = "POLICY_CREATE"
	ActionPolicyVersionCreate   = "POLICY_VERSION_CREATE"
	ActionPolicyVersionActivate = "POLICY_VERSION_ACTIVATE"
)

// HTTPClient implements Client against a real authorization-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://authorization-svc:8089" (no trailing slash required).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		// Tight timeout — a policy mutation must not stall indefinitely
		// because authorization-svc is slow.
		http: &http.Client{Timeout: 2 * time.Second},
	}
}

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

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
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
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", actionType),
			zap.ByteString("body", respBody),
		)
		return domain.ErrAuthorizationServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.log.Error("unreadable decision body from authorization-svc — failing closed",
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		return domain.ErrAuthorizationServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		c.log.Info("authorization denied",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.String("decision_basis", out.DecisionBasis),
			zap.String("access_decision_id", out.AccessDecisionID),
		)
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// PermitAllClient is the local-development stub. NewClient refuses to build
// it outside local development, so it can never be what a production
// deployment silently falls back to.
type PermitAllClient struct{ log *zap.Logger }

func NewPermitAllClient(log *zap.Logger) *PermitAllClient { return &PermitAllClient{log: log} }

func (c *PermitAllClient) CheckAllowed(_ context.Context, principalID, _, actionType string) error {
	c.log.Debug("authz stub — permitted (local development only)",
		zap.String("principal_id", principalID),
		zap.String("action_type", actionType),
	)
	return nil
}

// devPlaceholderURLs are AUTHZ_SERVICE_URL values that mean "nobody wired
// this yet".
var devPlaceholderURLs = map[string]bool{
	"":                         true,
	"http://authorization-svc": true,
}

// NewClient picks the client for the environment, refusing to start
// production or staging without a real Authorization Service.
func NewClient(env, baseURL string, log *zap.Logger) (Client, error) {
	isProdOrStaging := strings.EqualFold(env, "production") || strings.EqualFold(env, "staging")
	isPlaceholder := devPlaceholderURLs[strings.TrimRight(baseURL, "/")]

	if isProdOrStaging && isPlaceholder {
		return nil, fmt.Errorf("security violation: AUTHZ_SERVICE_URL (%q) is a placeholder in %s environment", baseURL, env)
	}
	if !isPlaceholder {
		log.Info("using HTTP authorization client", zap.String("url", baseURL))
		return NewHTTPClient(baseURL, log), nil
	}
	log.Warn("using PERMIT-ALL authorization stub — wire real AuthZ before production")
	return NewPermitAllClient(log), nil
}
