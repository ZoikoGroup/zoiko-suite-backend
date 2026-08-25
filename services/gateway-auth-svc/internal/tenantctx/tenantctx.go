// Package tenantctx resolves authoritative tenant and legal-entity context at
// the gateway, before a request reaches any backend service.
//
// This is GOV-01's job as ZS-SVC-A-001 §4 defines it: "resolve the trusted
// tenant and operating context for every request, command, event, batch and
// support session before business processing begins", with authoritative
// ownership of "TenantContextResolution, TenantRoutingHint cache, context
// decision evidence" and explicitly NOT of tenant master data. The master lives
// in tenant-entity-registry-svc; this package reads it and decides.
//
// # WHY IT SITS HERE AND NOT IN EACH SERVICE
//
// envelope.Resolver has existed in services/_contract since the input-contract
// pass and was wired into nothing — every service took the caller's
// X-Jurisdiction-Context at face value and no service checked whether the
// tenant was allowed to transact at all. Resolving once at the gateway is what
// the §4 provenance model requires: class S values are "server-resolved" and a
// caller "may request context but cannot override result", which is only true
// if something upstream of the caller does the resolving.
//
// # FAILURE POSTURE
//
// GOV-01's degradation contract is precise: "Ambiguous or unresolved tenant =>
// deny; stale context may be read only within bounded TTL for non-material
// reads; no fallback to global/default tenant." That is implemented literally —
// see Resolve's error returns and the stale-grace path.
package tenantctx

import (
	"context"
	"errors"
	"sync"
	"time"

	"zoiko.io/gateway-auth-svc/internal/envelope"
)

// Errors surfaced to the caller. They are distinguished because the gateway
// answers them differently: a tenant that may not transact is a decision (403),
// while a registry that could not be reached is an inability to decide (503).
var (
	// ErrDenied means the registry answered and the answer was no: unknown
	// tenant, suspended tenant, or an entity belonging to a different tenant.
	ErrDenied = errors.New("tenant context denied")

	// ErrUnavailable means the registry could not be consulted, so no decision
	// exists. Never collapse this into ErrDenied — and never into an allow.
	ErrUnavailable = errors.New("tenant context unavailable")
)

// Context is the authoritative context resolved for one request.
type Context struct {
	envelope.ResolvedContext

	// Stale reports that this was served from an expired cache entry because
	// the registry was unreachable. Only ever true for non-material reads.
	Stale bool
}

// Resolver answers tenant-context questions with a bounded TTL cache.
//
// The cache is what makes this affordable: without it every gated request on
// the platform would add two registry round-trips, and the registry would
// become a hard synchronous dependency of literally everything. GOV-01
// anticipates exactly this and calls for a "multi-region local read cache" in
// its NFR profile.
type Resolver struct {
	inner *envelope.Resolver
	ttl   time.Duration

	// staleGrace bounds how long past ttl an entry may still answer a read
	// while the registry is unreachable. Zero disables stale reads entirely.
	staleGrace time.Duration

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	ctx      envelope.ResolvedContext
	resolved time.Time
}

// New builds a Resolver against tenant-entity-registry-svc.
//
// A nil Resolver is valid and resolves nothing — see Resolve. That is what
// gateway-auth-svc uses when TENANT_REGISTRY_URL is unset, matching how
// carta-svc and siem-integration-svc are already made optional in this service:
// an unconfigured dependency degrades to "not consulted", never to a fabricated
// answer in either direction.
func New(registryBaseURL string, ttl, staleGrace time.Duration) *Resolver {
	if registryBaseURL == "" {
		return nil
	}
	return &Resolver{
		inner:      envelope.NewResolver(registryBaseURL),
		ttl:        ttl,
		staleGrace: staleGrace,
		cache:      make(map[string]entry),
	}
}

// Enabled reports whether context resolution is configured.
func (r *Resolver) Enabled() bool { return r != nil }

// Resolve returns the authoritative context for tenantID/legalEntityID.
//
// materialWrite selects the failure posture. Traefik's ForwardAuth passes the
// client's original method through to /verify, so the gateway genuinely knows
// whether the request it is authorising changes state — and a write is never
// allowed to proceed on a context nobody could confirm.
func (r *Resolver) Resolve(ctx context.Context, tenantID, legalEntityID string, materialWrite bool) (Context, error) {
	if r == nil {
		return Context{}, nil
	}
	if tenantID == "" {
		// No fallback to a global or default tenant. GOV-01 negative path #2.
		return Context{}, ErrDenied
	}

	key := tenantID + "\x00" + legalEntityID

	if cached, ok := r.fresh(key); ok {
		return Context{ResolvedContext: cached}, nil
	}

	env := envelope.Envelope{TenantID: tenantID, LegalEntityID: legalEntityID}
	resolved, err := r.inner.Resolve(ctx, env)
	if err == nil {
		r.store(key, resolved)
		return Context{ResolvedContext: resolved}, nil
	}

	// A registry that answered "no" is a decision, and decisions are not
	// cached: a tenant reactivated a second ago must not stay locked out for
	// the length of a TTL, and the read that would notice is this one.
	if errors.Is(err, envelope.ErrTenantNotResolvable) || errors.Is(err, envelope.ErrEntityNotResolvable) {
		r.invalidate(key)
		return Context{}, ErrDenied
	}

	// Registry unreachable. A read may proceed on a bounded-stale entry; a
	// material write may not, because the state it would change is exactly what
	// the unconfirmed context governs.
	if !materialWrite {
		if stale, ok := r.stale(key); ok {
			return Context{ResolvedContext: stale, Stale: true}, nil
		}
	}
	return Context{}, ErrUnavailable
}

func (r *Resolver) fresh(key string) (envelope.ResolvedContext, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.cache[key]
	if !ok || time.Since(e.resolved) > r.ttl {
		return envelope.ResolvedContext{}, false
	}
	return e.ctx, true
}

func (r *Resolver) stale(key string) (envelope.ResolvedContext, bool) {
	if r.staleGrace <= 0 {
		return envelope.ResolvedContext{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.cache[key]
	if !ok || time.Since(e.resolved) > r.ttl+r.staleGrace {
		return envelope.ResolvedContext{}, false
	}
	return e.ctx, true
}

func (r *Resolver) store(key string, ctx envelope.ResolvedContext) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = entry{ctx: ctx, resolved: time.Now()}
}

func (r *Resolver) invalidate(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, key)
}

// Invalidate drops any cached context for a tenant/entity pair. Exposed so a
// future GOV-01 InvalidateTenantContext command has something to call; nothing
// invokes it yet, and it is a no-op on a disabled resolver.
func (r *Resolver) Invalidate(tenantID, legalEntityID string) {
	if r == nil {
		return
	}
	r.invalidate(tenantID + "\x00" + legalEntityID)
}
