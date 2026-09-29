// Package authz confirms, via authorization-svc, that a principal may perform
// a named action — both for this service's control-plane writes and, far more
// often, for the per-result re-authorization every R1/R2 hit needs.
//
// Doctrine (03-microservices.md §17.1): no service self-authorizes a material
// action. For a SEARCH service the stakes are higher than usual, because
// INV-05 and INV-06 say the index is not an authorization: "access can change
// after indexing, and a stale index hit must not preserve access that IAM,
// PRV, DRC or the domain has since revoked."
//
// This client FAILS CLOSED. An unreachable, slow or misbehaving
// authorization-svc suppresses the result; it never returns it. That is INV-07
// ("unknown or unavailable authorization fails closed for protected content")
// and NP-04 ("IAM decision unavailable → protected search returns
// INDETERMINATE/BLOCK, not broad results"), and it is the difference between a
// search outage and a search disclosure.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/search-indexer-svc/internal/envelope"
)

var (
	// ErrDenied is an explicit DENIED decision — a real answer. Maps to
	// SUPPRESS with reason ESR-010.
	ErrDenied = errors.New("authorization denied")
	// ErrUnavailable means NO decision could be obtained. Maps to
	// INDETERMINATE with reason ESR-008, and suppresses just the same — the
	// difference matters for the operator reading the evidence, not for the
	// caller reading the results.
	ErrUnavailable = errors.New("authorization service unavailable")
)

// Client is the narrow interface the query and retrieval layers depend on.
type Client interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// decisionCacheTTL bounds how long a decision may be reused.
//
// Shorter here than in most services (5s elsewhere, 2s here) for a reason
// specific to search: one search request re-authorizes up to a page of results
// at once, so the cache is absorbing a burst inside a single user action
// rather than spreading one decision across a session. Two seconds is long
// enough for that burst and short enough that NP-05's "index contains stale
// access attributes after role revocation" cannot be made worse by this cache
// than it already is by the index.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable
// authorization-svc is never cached: that would turn one transient outage into
// a standing answer for every subsequent caller on this instance, which is
// precisely what fail-closed is supposed to prevent.
const decisionCacheTTL = 2 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

// HTTPClient implements Client against a real authorization-svc.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger

	mu          sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		// Tight. A search that stalls on authorization is a search that times
		// out, and a timeout here suppresses rather than permits — so the cost
		// of a slow authorizer is paid in missing results, which must be
		// bounded and visible rather than open-ended.
		http:  &http.Client{Timeout: 2 * time.Second},
		cache: make(map[string]cachedDecision),
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse matches authorization-svc. Both GRANTED and DENIED come
// back as HTTP 200 — the status reflects "the evaluation succeeded", not the
// outcome — so the body must always be read.
type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := cacheKey(ctx, principalID, legalEntityID, actionType)
	if decision, hit := c.lookupCache(key); hit {
		return decision
	}
	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)
	if err == nil || errors.Is(err, ErrDenied) {
		c.storeCache(key, err)
	}
	return err
}

// cacheKey includes the tenant, because roles in authorization-svc are
// tenant-scoped and its own tables are under row-level security: the answer to
// "may this principal perform this action" genuinely differs per tenant.
// Keying without it stores an answer under a question it did not ask and
// serves it to the wrong tenant for the rest of the TTL — a defect
// secret-vault-integration-svc's client already had, confirmed failing in both
// directions.
func cacheKey(ctx context.Context, principalID, legalEntityID, actionType string) string {
	tenantID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		tenantID = env.TenantID
	}
	return tenantID + "|" + principalID + "|" + legalEntityID + "|" + actionType
}

func (c *HTTPClient) lookupCache(key string) (error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(d.expiresAt) {
		delete(c.cache, key)
		return nil, false
	}
	return d.deniedErr, true
}

func (c *HTTPClient) storeCache(key string, decision error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = cachedDecision{deniedErr: decision, expiresAt: time.Now().Add(decisionCacheTTL)}
	c.cacheWrites++
	if c.cacheWrites%1000 == 0 {
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expiresAt) {
				delete(c.cache, k)
			}
		}
	}
}

func (c *HTTPClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
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
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	// authorization-svc validates the same canonical envelope this service
	// does, and answers 400 envelope_incomplete without it. A non-200 is
	// treated as unavailable below, so an unforwarded envelope would turn
	// EVERY gated call into a 503 that reads like an outage rather than a
	// missing header — the defect 3c618c2 (HR) and dbf6e45 (notification)
	// both hit.
	//
	// The values are the CALLER's, from the envelope the middleware already
	// parsed. Minting fresh ones would satisfy the contract and lose the only
	// thing it is for: a decision in access_decision_log traceable to the
	// request that caused it.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	authzSourceChannel := "system"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.RequestID != "" {
			authzRequestID = env.RequestID
		}
		if env.SourceChannel != "" {
			authzSourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	// One decision per (request, action, entity). A search re-authorizes many
	// entities under one request id, and collapsing them onto one idempotency
	// key would record ONE decision for a page of twenty results — losing
	// exactly the per-resource audit trail TC-05 asks for.
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType+":"+legalEntityID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("action_type", actionType), zap.Error(err))
		return ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", actionType),
			zap.ByteString("body", respBody))
		return ErrUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.log.Error("unreadable decision body from authorization-svc — failing closed",
			zap.String("action_type", actionType), zap.Error(err))
		return ErrUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return ErrDenied
	}
	return nil
}

// PermitAllClient is the local-development stub. NewClient refuses to build it
// outside local development, so no deployment can silently fall back to it.
type PermitAllClient struct{ log *zap.Logger }

func NewPermitAllClient(log *zap.Logger) *PermitAllClient { return &PermitAllClient{log: log} }

func (c *PermitAllClient) CheckAllowed(_ context.Context, principalID, _, actionType string) error {
	c.log.Debug("authz stub — permitted (local development only)",
		zap.String("principal_id", principalID), zap.String("action_type", actionType))
	return nil
}

// DenyAllClient exists for tests that must prove a guard is real. A handler
// test that passes with PermitAllClient proves the happy path; only a denying
// stub proves the gate is wired at all.
type DenyAllClient struct{}

func (DenyAllClient) CheckAllowed(context.Context, string, string, string) error { return ErrDenied }

// devPlaceholderURLs are AUTHZ_SERVICE_URL values that mean "not wired yet".
var devPlaceholderURLs = map[string]bool{
	"":                         true,
	"http://authorization-svc": true,
	"http://localhost":         true,
}

// NewClient picks the real client or the stub, and refuses the stub outside
// local development.
//
// A misconfigured production deployment must not come up permitting
// everything. Returning an error here means it does not come up at all, which
// is the only safe direction for a service whose whole job is deciding what a
// caller may see.
func NewClient(env, baseURL string, log *zap.Logger) (Client, error) {
	if devPlaceholderURLs[strings.TrimRight(baseURL, "/")] {
		if env != "local" {
			return nil, fmt.Errorf(
				"AUTHZ_SERVICE_URL is unset or a development placeholder (%q) in env %q: "+
					"refusing to start with an authorization stub that permits every retrieval", baseURL, env)
		}
		log.Warn("authorization-svc is not configured — using the permit-all stub (local development only)")
		return NewPermitAllClient(log), nil
	}
	return NewHTTPClient(baseURL, log), nil
}
