package context

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// S1-B3 (GOV-01 s4 cache state): ingress bindings are refreshed from the tenant registry, which stays the master.
//
// Before this nothing ever wrote a binding, so every binding read STALE past its TTL (or was never there). Now a
// binding that is missing, STALE or INVALIDATED is re-read from the registry on use, and:
//
//	registry binds the host to a tenant   -> the binding is written (or rewritten) FRESH and used
//	registry says the host is not bound   -> a binding held here is deactivated: the registry removed it
//	registry cannot answer                -> nothing changes: the old rules apply (a STALE binding is admitted
//	                                         within policy, an INVALIDATED one is refused under strict). A refresh
//	                                         that fails never widens access.
//
// The refresh can only confirm or correct what the registry says. The asymmetry of CheckIngressBinding is untouched:
// a binding still never supplies a tenant the token did not claim.

// HostResolver reads the registry's ORG-02 ResolveTenantByHost. found=false is the registry's own 404, an answer;
// an error is an outage.
type HostResolver interface {
	ResolveHost(ctx context.Context, host string) (tenantID, lifecycleState string, found bool, err error)
}

// IngressBindingUpserter writes a refreshed binding. Never reachable from an HTTP route (see UpsertIngressBinding).
type IngressBindingUpserter interface {
	UpsertIngressBinding(ctx context.Context, b domain.TenantIngressBinding) error
}

type ingressRefresher struct {
	source      HostResolver
	writer      IngressBindingUpserter
	env         domain.Environment
	minInterval time.Duration // per identifier
	maxPerSec   int           // across identifiers, so a flood of made-up hosts cannot become a flood of registry calls
	now         func() time.Time

	mu       sync.Mutex
	last     map[string]time.Time
	winStart time.Time
	winCount int
}

const maxTrackedIdentifiers = 10000

// WithRefresh turns on refresh-through. minInterval bounds how often one identifier is re-read; maxPerSecond bounds
// registry reads overall. A non-positive minInterval, or an invalid environment, leaves refresh off.
func (c *IngressChecker) WithRefresh(source HostResolver, writer IngressBindingUpserter, env domain.Environment, minInterval time.Duration, maxPerSecond int) *IngressChecker {
	if source == nil || writer == nil || minInterval <= 0 || !env.Valid() {
		return c
	}
	if maxPerSecond <= 0 {
		maxPerSecond = 20
	}
	c.refresh = &ingressRefresher{source: source, writer: writer, env: env, minInterval: minInterval, maxPerSec: maxPerSecond,
		now: func() time.Time { return time.Now().UTC() }, last: map[string]time.Time{}}
	return c
}

// allow reports whether this identifier may be re-read now, and records the attempt.
func (r *ingressRefresher) allow(identifier string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if t, ok := r.last[identifier]; ok && now.Sub(t) < r.minInterval {
		return false
	}
	if now.Sub(r.winStart) >= time.Second {
		r.winStart, r.winCount = now, 0
	}
	if r.winCount >= r.maxPerSec {
		return false
	}
	r.winCount++
	if len(r.last) >= maxTrackedIdentifiers {
		r.last = map[string]time.Time{}
	}
	r.last[identifier] = now
	return true
}

// refreshed returns the binding to use after consulting the registry, or the one passed in when the registry could
// not be consulted. changed=false means the registry was not asked or could not answer.
func (c *IngressChecker) refreshed(ctx context.Context, identifier string, current *domain.TenantIngressBinding) (*domain.TenantIngressBinding, bool) {
	r := c.refresh
	if r == nil {
		return current, false
	}
	if current != nil && current.Freshness(r.now(), c.bindingTTL) == domain.CacheFresh {
		return current, false
	}
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if !r.allow(identifier) {
		return current, false
	}
	tenantID, lifecycle, found, err := r.source.ResolveHost(ctx, identifier)
	if err != nil {
		c.log.Warn("ingress binding refresh: registry could not answer; the held binding stands as it was",
			zap.String("ingress", identifier), zap.Error(err))
		return current, false
	}
	if !found {
		if current == nil {
			return nil, true
		}
		// The registry no longer binds this host. Deactivated, not deleted: the row is evidence of what was held.
		dead := *current
		dead.ActiveFlag = false
		dead.SourceVersion = "registry:unbound@" + r.now().Format(time.RFC3339)
		if err := r.writer.UpsertIngressBinding(ctx, dead); err != nil {
			c.log.Error("ingress binding refresh: could not deactivate a binding the registry removed",
				zap.String("ingress", identifier), zap.Error(err))
			return current, false
		}
		c.log.Warn("ingress binding deactivated: the registry no longer binds this host",
			zap.String("ingress", identifier), zap.String("tenant_id", current.TenantID))
		return nil, true
	}
	if tenantID == "" {
		return current, false
	}
	now := r.now()
	fresh := domain.TenantIngressBinding{IngressIdentifier: identifier, TenantID: tenantID, Environment: r.env, ActiveFlag: true,
		// The registry exposes no revision number for a binding, so the version names what was read and when.
		SourceVersion: fmt.Sprintf("registry:resolve-tenant:%s@%s", lifecycle, now.Format(time.RFC3339)), RefreshedAt: now}
	if err := r.writer.UpsertIngressBinding(ctx, fresh); err != nil {
		c.log.Error("ingress binding refresh: could not store the refreshed binding", zap.String("ingress", identifier), zap.Error(err))
		return current, false
	}
	if current != nil && current.TenantID != tenantID {
		c.log.Error("INGRESS BINDING CORRECTED: the registry binds this host to a different tenant than the one held",
			zap.String("ingress", identifier), zap.String("held_tenant_id", current.TenantID), zap.String("registry_tenant_id", tenantID))
	}
	return &fresh, true
}
