package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	svcenvelope "zoiko.io/mtls-management-svc/internal/envelope"
)

var (
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
	ErrAuthorizationDenied     = errors.New("authorization denied")
	// ErrAuthorizationRejected: authorization-svc refused to evaluate the
	// question as asked (400) — a malformed legal_entity_id, say. Not an
	// outage, and not cached: nothing was decided.
	ErrAuthorizationRejected = errors.New("authorization request rejected")
)

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

// errRefusedUncached marks a denial that must not be cached (see the 403 case
// in checkAllowedLive).
var errRefusedUncached = errors.New("refused before evaluation")

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	baseURL string
	client  *http.Client

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
		cache:   make(map[string]cachedDecision),
	}
}

// NewClientWithHTTPClient is NewClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL: baseURL,
		client:  httpClient,
		cache:   make(map[string]cachedDecision),
	}
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType within legalEntityID, failing closed: nil only on GRANTED.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	// The tenant is part of the question authorization-svc answers (it
	// refuses an entity outside the forwarded tenant), so it is part of the
	// key. It was not: a GRANTED for tenant A was replayed, for up to the TTL,
	// to the same principal/entity/action asked under tenant B — found live
	// on 13 Oct 2026, where a wrong-tenant listing answered 200.
	tenantID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		tenantID = env.TenantID
	}
	key := tenantID + "|" + principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	if err == nil || (errors.Is(err, ErrAuthorizationDenied) && !errors.Is(err, errRefusedUncached)) {
		c.storeCache(key, err)
	}

	return err
}

// lookupCache returns the cached decision for key and whether it is still
// within decisionCacheTTL. An expired entry is evicted on read.
func (c *Client) lookupCache(key string) (error, bool) {
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
// combinations doesn't grow the map unboundedly between reads of the same
// key.
func (c *Client) storeCache(key string, decision error) {
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

// checkAllowedLive calls authorization-svc's POST /v1/authorize endpoint and
// enforces fail-closed behavior: any transport error, non-200 response, or
// decision other than "GRANTED" results in the action being refused.
func (c *Client) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// authorization-svc's resolveTenantScope prefers the verified X-Tenant-Id
	// header over the request body, and — per its own doc comment — silently
	// narrows to global-only SoD rules when neither is present, rather than
	// refusing. Forwarding it here is what makes this tenant's
	// segregation-of-duties rules actually apply to decisions made through
	// this service, not just the globally-applicable ones. Same defect,
	// same fix as board-resolutions-svc.
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
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)

	resp, err := c.client.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		// authorization-svc refuses some questions before evaluating them —
		// tenant_scope_mismatch (the entity is another tenant's),
		// tenant_required, a principal it has suspended at the gate. Those
		// are denials. They used to be reported as 503 "authorization service
		// unavailable", so a wrong-tenant request looked like an outage.
		// Not cached: the refusal is about this request's scope, and a later
		// projection or registry answer can change it.
		return fmt.Errorf("%w: %w", ErrAuthorizationDenied, errRefusedUncached)
	case http.StatusBadRequest:
		return ErrAuthorizationRejected
	default:
		// 401 (this service's workload identity refused), 5xx and anything
		// else: no decision was made. Fail closed.
		return ErrAuthzServiceUnavailable
	}

	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrAuthzServiceUnavailable
	}

	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}

	return nil
}
