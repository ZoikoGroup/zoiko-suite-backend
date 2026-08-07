// Package middleware provides tenant-context propagation for
// governance-decision-log-svc, mirroring the X-Tenant-Id convention already
// used elsewhere in the platform (e.g. employee-master-svc) and already sent
// by real callers of this service (decision-support-svc's GovernanceLogClient
// sets X-Tenant-Id on every request today; this service simply never read it).
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext extracts X-Tenant-Id into the request context. It does not
// reject requests missing the header — handlers that require a tenant scope
// enforce that themselves, since some routes (none currently) may be
// legitimately tenant-agnostic.
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				r = r.WithContext(WithTenant(r.Context(), tenantID))
			}
			next.ServeHTTP(w, r)
		})
	}
}
