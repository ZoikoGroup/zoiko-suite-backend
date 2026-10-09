package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// EntitlementClient resolves GOV-01 §4's entitlement context reference from
// COM-03 Entitlement (commercial-account-svc, ZS-SVC-Q-001 §4.3), which now
// exists: GET /v1/commercial/entitlements/effective.
//
// The reference is a deterministic name for the commercial state the session
// was resolved under:
//
//	com03:sub=<subscription_id>;policy=<policy_version>;restr=<restriction ids>;d=<digest>
//
// where the digest is SHA-256 over the sorted (capability, outcome, limit)
// triples. Two resolutions under the same entitlements record the same
// reference; any change to the plan, a limit, the policy or a restriction
// changes it, and COM-03's append-only history can be read back against it.
//
// Called as this service's own identity naming the organization, which needs
// the platform COMMERCIAL_RESTRICTION_READ grant (seed-demo-rbac.ps1). Never
// blocks a login: the resolver records UPSTREAM_UNAVAILABLE on any error. A
// short per-tenant cache keeps the commercial plane off the login hot path.
type EntitlementClient struct {
	baseURL   string
	principal string
	client    *http.Client
	ttl       time.Duration

	mu    sync.Mutex
	cache map[string]cachedRef
}

type cachedRef struct {
	ref string
	at  time.Time
}

func NewEntitlementClient(baseURL, servicePrincipal string, ttl time.Duration) *EntitlementClient {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &EntitlementClient{
		baseURL: strings.TrimRight(baseURL, "/"), principal: servicePrincipal,
		client: &http.Client{Timeout: 2 * time.Second}, ttl: ttl, cache: map[string]cachedRef{},
	}
}

type comDecision struct {
	CapabilityKey        string  `json:"capability_key"`
	Outcome              string  `json:"outcome"`
	LimitValue           *int64  `json:"limit_value"`
	LimitUnit            *string `json:"limit_unit"`
	AppliedRestrictionID *string `json:"applied_restriction_id"`
	PolicyVersion        *int    `json:"policy_version"`
	SubscriptionID       *string `json:"subscription_id"`
}

// ResolveEntitlementContext implements identityctx.EntitlementResolver.
func (c *EntitlementClient) ResolveEntitlementContext(ctx context.Context, tenantID, _ string) (*string, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: no tenant to resolve entitlements for", ErrUpstreamRead)
	}
	c.mu.Lock()
	if e, ok := c.cache[tenantID]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		ref := e.ref
		return &ref, nil
	}
	c.mu.Unlock()

	u := c.baseURL + "/v1/commercial/entitlements/effective?organization_id=" + url.QueryEscape(tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstreamRead, err)
	}
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Workload-Id", "identity-context-svc")
	req.Header.Set("X-Request-Id", ulid.Make().String())
	req.Header.Set("X-Correlation-ID", ulid.Make().String())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: commercial-account-svc unreachable: %v", ErrUpstreamRead, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: commercial-account-svc answered %d", ErrUpstreamRead, resp.StatusCode)
	}
	var body struct {
		Decisions []comDecision `json:"decisions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: undecodable entitlements: %v", ErrUpstreamRead, err)
	}
	ref := EntitlementReference(body.Decisions)
	c.mu.Lock()
	c.cache[tenantID] = cachedRef{ref: ref, at: time.Now()}
	c.mu.Unlock()
	return &ref, nil
}

// EntitlementReference builds the deterministic reference from COM-03's
// effective decisions. Exported for tests.
func EntitlementReference(ds []comDecision) string {
	sub, policy := "none", "none"
	restr := map[string]bool{}
	lines := make([]string, 0, len(ds))
	for _, d := range ds {
		if d.SubscriptionID != nil && *d.SubscriptionID != "" {
			sub = *d.SubscriptionID
		}
		if d.PolicyVersion != nil {
			policy = strconv.Itoa(*d.PolicyVersion)
		}
		if d.AppliedRestrictionID != nil && *d.AppliedRestrictionID != "" {
			restr[*d.AppliedRestrictionID] = true
		}
		limit := "-"
		if d.LimitValue != nil {
			limit = strconv.FormatInt(*d.LimitValue, 10)
			if d.LimitUnit != nil {
				limit += *d.LimitUnit
			}
		}
		lines = append(lines, d.CapabilityKey+"="+d.Outcome+"/"+limit)
	}
	sort.Strings(lines)
	ids := make([]string, 0, len(restr))
	for id := range restr {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "com03:sub=" + sub + ";policy=" + policy + ";restr=" + strings.Join(ids, ",") + ";d=" + hex.EncodeToString(sum[:8])
}
