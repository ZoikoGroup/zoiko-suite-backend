// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to mutate this service's identity data.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/identity-context-svc/internal/envelope"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil if principalID is authorized to perform
	// actionType within legalEntityID. Returns domain.ErrAuthorizationDenied
	// on a DENIED decision, or domain.ErrAuthorizationServiceUnavailable if
	// no decision could be obtained — callers must fail closed on the latter.
	// tenantID is the caller's verified tenant scope (from X-Tenant-Id header).
	// Empty string means no tenant scope (global-only SoD rules).
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error
}

const actionPrincipalStatusManage = "PRINCIPAL_STATUS_MANAGE"

const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

// HTTPClient implements Client against a real authorization-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://authorization-svc:8089" (no trailing slash required).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		http:    &http.Client{Timeout: authzTimeout()},
		cache:   make(map[string]cachedDecision),
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	TenantID      string `json:"tenant_id,omitempty"`
}

// authorizeResponse matches authorization-svc's response. Both GRANTED and
// DENIED come back as HTTP 200 — the status reflects "the evaluation
// succeeded", not the outcome — so the body must always be read.
type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType + "|" + tenantID

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType, tenantID)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
		c.storeCache(key, err)
	}

	return err
}

// lookupCache returns the cached decision for key and whether it is still
// within decisionCacheTTL. An expired entry is evicted on read.
func (c *HTTPClient) lookupCache(key string) (error, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

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

// storeCache records a real GRANTED/DENIED decision. Every 1000th write
// sweeps expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations doesn't grow the map
// unboundedly between reads of the same key.
func (c *HTTPClient) storeCache(key string, decision error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

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

// checkAllowedLive is the real, uncached call to authorization-svc.
func (c *HTTPClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
		TenantID:      tenantID,
	})
	if err != nil {
		return fmt.Errorf("marshal authorize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	// Forward the caller's canonical envelope. Without this the outbound request
	// carried Content-Type and nothing else, authorization-svc refused it with 401
	// envelope_incomplete, and this client turned that into "authorization service
	// unavailable" -- failing closed on every authorized write while
	// authorization-svc was healthy and answering correctly.
	svcenvelope.ForwardTo(ctx, req)

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

func (c *PermitAllClient) CheckAllowed(_ context.Context, principalID, _, actionType, _ string) error {
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
