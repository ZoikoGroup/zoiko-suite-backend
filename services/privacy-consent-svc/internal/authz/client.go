package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-consent-svc/internal/middleware"
)

// platformScopeID mirrors handler.platformScopeID / authorization-svc's
// platform entity UUID. Duplicated here (not imported from package handler,
// which imports this package) rather than factored into a third package,
// since it is one literal shared by exactly two files.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
var ErrAuthorizationDenied = errors.New("authorization denied")

const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	httpClient *http.Client
	baseURL    string

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		cache:      make(map[string]cachedDecision),
	}
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType within legalEntityID's scope. tenantID is read from the
// caller's verified request context (see checkAllowedLive) — it is a
// SEPARATE dimension from legalEntityID/scope, not derived from it. A
// platform-wide action (legalEntityID == platformScopeID, or empty) still
// happens inside the caller's own real tenant; the call previously
// substituted a single hard-coded tenant UUID for every platform-scope
// check across every tenant on the platform, which meant authorization-svc
// evaluated the grant against roles assigned under that one fake tenant
// rather than the caller's own — scoped correctly for a tenant-scoped
// action, silently wrong for a platform-scoped one from any tenant other
// than the hard-coded one.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	if err == nil || errors.Is(err, ErrAuthorizationDenied) {
		c.storeCache(key, err)
	}

	return err
}

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

func (c *Client) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	// X-Tenant-Id is the caller's own VERIFIED tenant, read from context —
	// never derived from legalEntityID/scope. A platform-scope check
	// (legalEntityID == platformScopeID, or empty) still happens inside the
	// caller's real tenant; only when no tenant is known at all (a true
	// bootstrap call, e.g. before any tenant context exists) does this fall
	// back to the platform scope itself, which is a real, resolvable id —
	// never the unrelated hard-coded tenant this used to send for every
	// platform-scope check regardless of which tenant the caller belonged to.
	tenantID := middleware.TenantFromContext(ctx)
	if tenantID == "" {
		tenantID = platformScopeID
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Request-Id", uuid.New().String())
	req.Header.Set("X-Source-Channel", "web")
	req.Header.Set("Idempotency-Key", uuid.New().String())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
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
