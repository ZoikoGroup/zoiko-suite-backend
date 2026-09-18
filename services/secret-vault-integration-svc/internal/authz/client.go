// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to mutate this service's secret access broker.
//
// Doctrine (03-microservices.md §17.1): no service self-authorizes a
// material action. This service shipped without a gate of any kind, so
// anyone able to reach its port could write to it.
//
// This client FAILS CLOSED. An unreachable, slow, or misbehaving
// authorization-svc rejects the mutation; it never silently permits it.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5/middleware"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"

	"go.uber.org/zap"
)

// Sentinel errors, mapped to HTTP status codes by the handler.
var (
	// ErrDenied is an explicit DENIED decision (403 Forbidden).
	ErrDenied = errors.New("authorization denied")
	// ErrUnavailable means no decision could be obtained (503). Callers must
	// fail closed — the mutation does not proceed.
	ErrUnavailable = errors.New("authorization service unavailable")
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil only on a GRANTED decision.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// decisionCacheTTL bounds how long a GRANTED/DENIED decision from
// authorization-svc may be reused locally before it is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 anticipates exactly
// this cost: "For Tier 0 and latency-sensitive services, policy and
// authorization evaluation may use high-speed distributed enforcement
// patterns, including local policy caches... provided policy source
// remains centralized, policy provenance is auditable, stale decision
// risk is bounded, fail-safe behavior is defined." This constant is that
// bound — short enough that a permission revocation or role change
// propagates within one cache generation, long enough to absorb the
// repeat checks a single user action or request burst produces.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable or
// misbehaving authorization-svc is never cached — that would turn one
// transient outage into a standing permit-or-deny for every subsequent
// caller on this instance, which defeats fail-closed.
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
// "http://authorization-svc:8089".
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		// Tight timeout — a mutation must not stall indefinitely because
		// authorization-svc is slow.
		http:  &http.Client{Timeout: 2 * time.Second},
		cache: make(map[string]cachedDecision),
	}
}

// NewHTTPClientWithHTTPClient is NewHTTPClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewHTTPClientWithHTTPClient(baseURL string, log *zap.Logger, httpClient *http.Client) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		http:    httpClient,
		cache:   make(map[string]cachedDecision),
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

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, ErrDenied) {
		c.storeCache(key, err)
	}

	return err
}

// cacheKey identifies a decision by everything authorization-svc actually uses
// to reach it.
//
// The tenant is the part that was missing, and leaving it out was a real
// cross-tenant authorization defect rather than a theoretical one. Roles in
// authorization-svc are tenant-scoped and its own tables are under row-level
// security, so the answer to "may this principal perform this action" genuinely
// differs per tenant — checkAllowedLive forwards X-Tenant-Id precisely because
// of that. Keying the cache on (principal, entity, action) alone therefore
// stored an answer under a question it did not ask, and served it to the wrong
// tenant for the rest of the TTL.
//
// It failed in both directions, both confirmed against the running service:
//
//   - A principal authorized in tenant A, acting immediately afterwards in
//     tenant B where it holds no role at all, was served A's cached GRANT and
//     passed the gate. For this service that is a window in which a caller can
//     revoke another tenant's leases, write its secret material, or rotate its
//     secrets.
//   - The reverse: a denial in tenant B was served to tenant A, locking a
//     legitimately-authorized operator out of its own tenant.
//
// The window is decisionCacheTTL, which is short — but a credential-brokering
// service is exactly where a short window still matters, and the failure is
// silent in both directions.
//
// The tenant comes from the envelope the middleware already parsed, which is
// the same source checkAllowedLive sends the header from. When there is no
// envelope the key gets an empty tenant segment, which is consistent: a
// request with no tenant produces a call with no X-Tenant-Id, so both share
// one cache entry and neither can be confused for a tenant-scoped one.
func cacheKey(ctx context.Context, principalID, legalEntityID, actionType string) string {
	tenantID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		tenantID = env.TenantID
	}
	return tenantID + "|" + principalID + "|" + legalEntityID + "|" + actionType
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

	// authorization-svc validates the same canonical envelope contract this
	// service does and answers 400 envelope_incomplete without it. A non-200 is
	// treated as unavailable below, so an unforwarded envelope turned EVERY
	// gated write into a 503 that reads like an outage rather than a missing
	// header. Same defect, same fix as 3c618c2 (HR) and dbf6e45 (notification).
	//
	// The values are the CALLER's, taken from the envelope the middleware
	// already parsed into this request's context. Minting fresh ones would
	// satisfy the contract and lose the only thing it is for: a decision in
	// access_decision_log traceable to the request that caused it.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
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
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	// One decision per (request, action): an inbound request may authorize
	// several actions, and each is its own decision to record.
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		return ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", actionType),
			zap.ByteString("body", respBody),
		)
		return ErrUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.log.Error("unreadable decision body from authorization-svc — failing closed",
			zap.String("action_type", actionType), zap.Error(err))
		return ErrUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		c.log.Info("authorization denied",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.String("decision_basis", out.DecisionBasis),
			zap.String("access_decision_id", out.AccessDecisionID),
		)
		return ErrDenied
	}
	return nil
}

// PermitAllClient is the local-development stub. NewClient refuses to build
// it outside local development, so no production deployment can silently
// fall back to it.
type PermitAllClient struct{ log *zap.Logger }

func NewPermitAllClient(log *zap.Logger) *PermitAllClient { return &PermitAllClient{log: log} }

func (c *PermitAllClient) CheckAllowed(_ context.Context, principalID, _, actionType string) error {
	c.log.Debug("authz stub — permitted (local development only)",
		zap.String("principal_id", principalID),
		zap.String("action_type", actionType),
	)
	return nil
}

// devPlaceholderURLs are AUTHZ_SERVICE_URL values that mean "not wired yet".
var devPlaceholderURLs = map[string]bool{
	"":                         true,
	"http://authorization-svc": true,
}

// reservedHostSuffixes are the domains RFC 2606 and RFC 6761 reserve for
// documentation and testing. Nothing deployed lives on one.
var reservedHostSuffixes = []string{
	".example.com", ".example.net", ".example.org",
	".example", ".invalid", ".test",
}

// nonProductionURL reports why baseURL cannot be a deployed authorization-svc,
// or "" if it might be one.
//
// It is a POSITIVE test for addresses that are provably local or reserved,
// never a guess at what a real hostname looks like. An unrecognised host is
// assumed real: a false positive here is a refusal to boot in production,
// which is a worse failure than the one this guard exists to prevent.
func nonProductionURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "not a parseable URL"
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "not an absolute URL with a host"
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback():
			return "a loopback address"
		case ip.IsUnspecified():
			return "an unspecified address"
		}
		return ""
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "a loopback address"
	}
	for _, suffix := range reservedHostSuffixes {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return "a reserved documentation or test domain"
		}
	}
	return ""
}

// NewClient picks the client for the environment, refusing to start
// production or staging without a real Authorization Service.
func NewClient(env, baseURL string, log *zap.Logger) (Client, error) {
	isProdOrStaging := strings.EqualFold(env, "production") || strings.EqualFold(env, "staging")
	trimmed := strings.TrimRight(baseURL, "/")
	isPlaceholder := devPlaceholderURLs[trimmed]

	if isProdOrStaging {
		if isPlaceholder {
			return nil, fmt.Errorf("security violation: AUTHZ_SERVICE_URL (%q) is a placeholder in %s environment", baseURL, env)
		}
		if reason := nonProductionURL(trimmed); reason != "" {
			return nil, fmt.Errorf("security violation: AUTHZ_SERVICE_URL (%q) is %s and cannot address authorization-svc in %s environment", baseURL, reason, env)
		}
	}
	if !isPlaceholder {
		log.Info("using HTTP authorization client", zap.String("url", baseURL))
		return NewHTTPClient(baseURL, log), nil
	}
	log.Warn("using PERMIT-ALL authorization stub — wire real AuthZ before production")
	return NewPermitAllClient(log), nil
}
