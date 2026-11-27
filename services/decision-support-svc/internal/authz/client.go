// Package authz asks authorization-svc whether a principal may perform a
// decision-support action. It fails closed: an unreachable or misbehaving
// authorization-svc is treated as "not allowed" by every caller.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/decision-support-svc/internal/domain"
)

// decisionCacheTTL bounds how long a GRANTED/DENIED decision may be reused
// locally before authorization-svc is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 allows "local policy
// caches ... provided ... stale decision risk is bounded, fail-safe behavior
// is defined." Five seconds is that bound: a revoked role stops working
// within one cache generation (ZS-SVC-X-001 NP-35 "Reviewer accepts output
// after losing required role -> role revalidated at action time").
//
// Only real GRANTED/DENIED decisions are cached, never an outage.
const decisionCacheTTL = 5 * time.Second

// Request is one authorization question.
type Request struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	ActionType    string
	CorrelationID string
	RequestID     string
}

func (r Request) cacheKey() string {
	// The tenant is part of the key: without it, two tenants that happen to
	// use the same legal-entity and principal identifiers would share a
	// cached decision.
	return r.TenantID + "|" + r.PrincipalID + "|" + r.LegalEntityID + "|" + r.ActionType
}

type cachedDecision struct {
	result    error // nil = GRANTED, ErrAuthorizationDenied = DENIED
	expiresAt time.Time
}

// Client calls authorization-svc's POST /v1/authorize.
type Client struct {
	baseURL string
	http    *http.Client

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

// NewClient builds a Client for baseURL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
		cache:   make(map[string]cachedDecision),
	}
}

// CheckAllowed returns nil when GRANTED, domain.ErrAuthorizationDenied when
// DENIED, and domain.ErrAuthzServiceUnavailable for anything else.
func (c *Client) CheckAllowed(ctx context.Context, req Request) error {
	key := req.cacheKey()

	if cached, hit := c.lookupCache(key); hit {
		return cached
	}

	result := c.checkAllowedLive(ctx, req)

	isRealDecision := result == nil || result == domain.ErrAuthorizationDenied
	if isRealDecision {
		c.storeCache(key, result)
	}
	return result
}

func (c *Client) lookupCache(key string) (error, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	entry, found := c.cache[key]
	if !found {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.cache, key)
		return nil, false
	}
	return entry.result, true
}

// storeCache records a decision. Every 1000th write sweeps expired entries
// so the map cannot grow without bound on a long-lived instance.
func (c *Client) storeCache(key string, result error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	c.cache[key] = cachedDecision{result: result, expiresAt: time.Now().Add(decisionCacheTTL)}

	c.cacheWrites++
	if c.cacheWrites%1000 != 0 {
		return
	}
	now := time.Now()
	for k, v := range c.cache {
		if now.After(v.expiresAt) {
			delete(c.cache, k)
		}
	}
}

// checkAllowedLive is the real, uncached call.
func (c *Client) checkAllowedLive(ctx context.Context, req Request) error {
	body, _ := json.Marshal(map[string]string{
		"tenant_id":       req.TenantID,
		"principal_id":    req.PrincipalID,
		"legal_entity_id": req.LegalEntityID,
		"action_type":     req.ActionType,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthzServiceUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", req.TenantID)
	httpReq.Header.Set("X-Principal-Id", req.PrincipalID)
	httpReq.Header.Set("X-Legal-Entity-Id", req.LegalEntityID)
	httpReq.Header.Set("X-Correlation-ID", req.CorrelationID)
	httpReq.Header.Set("X-Request-Id", req.RequestID)
	httpReq.Header.Set("X-Source-Channel", "system")
	// The key names the exact question (request, principal, legal entity,
	// action). Without a request ID it must still be unique: a constant key
	// such as ":DECISION_SUPPORT_VIEW" would let authorization-svc replay one
	// principal's decision to another.
	requestRef := req.RequestID
	if requestRef == "" {
		requestRef = uuid.NewString()
	}
	httpReq.Header.Set("Idempotency-Key", requestRef+":"+req.PrincipalID+":"+req.LegalEntityID+":"+req.ActionType)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return domain.ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var decision struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return domain.ErrAuthzServiceUnavailable
	}
	if decision.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}
