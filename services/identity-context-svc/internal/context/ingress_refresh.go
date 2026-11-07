package context

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// HostBinding is one tenant-entity-registry-svc host binding.
type HostBinding = domain.HostBinding

// HostBindingSource is the registry read the refresher depends on.
type HostBindingSource interface {
	ListTenantHostBindings(ctx context.Context, tenantID string) ([]HostBinding, error)
}

// IngressBindingSyncStore is the local binding table the refresher keeps current.
type IngressBindingSyncStore interface {
	ListIngressBindingTenants(ctx context.Context) ([]string, error)
	UpsertIngressBinding(ctx context.Context, b domain.TenantIngressBinding) error
	DeactivateIngressBindingsExcept(ctx context.Context, tenantID string, keep []string) (int, error)
}

// IngressRefresher re-reads every bound tenant's host bindings from the
// registry before their TTL lapses (S1-B3), so a binding reads STALE only
// while the registry is unreachable, never merely because nothing refreshed
// it. A hostname the registry no longer lists for the tenant is deactivated:
// a removed binding must stop routing, not linger until someone notices.
type IngressRefresher struct {
	store    IngressBindingSyncStore
	registry HostBindingSource
	env      domain.Environment
	ttl      time.Duration
	log      *zap.Logger
}

// NewIngressRefresher builds a refresher for bindings with the given TTL.
func NewIngressRefresher(store IngressBindingSyncStore, registry HostBindingSource, env domain.Environment,
	ttl time.Duration, log *zap.Logger) *IngressRefresher {
	return &IngressRefresher{store: store, registry: registry, env: env, ttl: ttl, log: log}
}

// RefreshInterval is half the binding TTL: every binding is refreshed at
// least once before it can turn STALE.
func RefreshInterval(ttl time.Duration) time.Duration { return ttl / 2 }

// refreshTimeout bounds one refresh cycle.
const refreshTimeout = 30 * time.Second

// Run refreshes once at start and then every RefreshInterval until ctx ends.
// Each cycle runs on a context detached from ctx's cancellation and bounded by
// refreshTimeout, so a shutdown mid-cycle never leaves a half-written tenant.
func (r *IngressRefresher) Run(ctx context.Context) {
	interval := RefreshInterval(r.ttl)
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cycle, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		n, err := r.RefreshOnce(cycle)
		cancel()
		if err != nil {
			r.log.Warn("ingress binding refresh incomplete", zap.Int("refreshed", n), zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RefreshOnce refreshes every tenant that has a local binding and reports
// how many bindings were written. A tenant whose registry read fails keeps its
// bindings as they are (they turn STALE at their TTL); the first such error is
// returned after every other tenant has been tried.
func (r *IngressRefresher) RefreshOnce(ctx context.Context) (int, error) {
	tenants, err := r.store.ListIngressBindingTenants(ctx)
	if err != nil {
		return 0, err
	}
	written := 0
	var firstErr error
	for _, tenantID := range tenants {
		bindings, err := r.registry.ListTenantHostBindings(ctx, tenantID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		keep := make([]string, 0, len(bindings))
		for _, b := range bindings {
			host := strings.ToLower(strings.TrimSpace(b.Hostname))
			if host == "" || b.TenantID != tenantID {
				continue // never let the registry's answer bind a host to another tenant
			}
			if err := r.store.UpsertIngressBinding(ctx, domain.TenantIngressBinding{
				IngressIdentifier: host, TenantID: tenantID, Environment: r.env,
				ActiveFlag: b.ActiveFlag, SourceVersion: "registry:" + b.HostBindingID,
			}); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			keep = append(keep, host)
			written++
		}
		if _, err := r.store.DeactivateIngressBindingsExcept(ctx, tenantID, keep); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return written, firstErr
}
