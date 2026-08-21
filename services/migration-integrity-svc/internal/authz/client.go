package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

var (
	// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
	// reached or returns an unexpected response. Callers must treat this as a
	// deny (fail closed).
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
	// ErrAuthorizationDenied is returned when authorization-svc explicitly
	// denies the requested action.
	ErrAuthorizationDenied = errors.New("authorization denied")
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

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	authzURL   string
	httpClient *http.Client
	logger     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(authzURL string, logger *zap.Logger) *Client {
	return &Client{authzURL: authzURL, httpClient: &http.Client{Timeout: 5 * time.Second}, logger: logger, cache: make(map[string]cachedDecision)}
}

// NewClientWithHTTPClient is NewClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewClientWithHTTPClient(authzURL string, httpClient *http.Client, logger *zap.Logger) *Client {
	return &Client{authzURL: authzURL, httpClient: httpClient, logger: logger, cache: make(map[string]cachedDecision)}
}

// Authorize delegates to Governance Plane Authorization Service
//
// Deprecated: this is a legacy no-op stub retained for backward
// compatibility. Use CheckAllowed, which performs a real fail-closed
// authorization check against authorization-svc.
func (c *Client) Authorize(ctx context.Context, tenantID, userID, action, resource string) (bool, error) {
	return true, nil
}

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and
// enforces a fail-closed policy: any transport error, non-200 response, or
// non-GRANTED decision results in a non-nil error and the action must not
// proceed.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, ErrAuthorizationDenied) {
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
// (principal, entity, action) combinations doesn't grow the map
// unboundedly between reads of the same key.
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

// checkAllowedLive is the real, uncached call to authorization-svc.
func (c *Client) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authzURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("failed to call authorization-svc", zap.Error(err))
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("authorization-svc returned non-200 status", zap.Int("status_code", resp.StatusCode))
		return ErrAuthzServiceUnavailable
	}

	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}

	return nil
}
