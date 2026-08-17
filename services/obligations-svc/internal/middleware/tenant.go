// Package middleware carries the caller's tenant from the request headers onto
// the context, where the store reads it.
//
// This service had no tenant dimension at all before 000002 — not a missing
// filter, a missing concept. Every read returned every tenant's obligations,
// and the idempotent-create path could answer with another tenant's record.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

// WithTenant is the context constructor the store and the tests share, so
// nothing outside this package needs to know the key type.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the caller's tenant, or "" when the request carried
// no tenant scope.
//
// Deliberately no default. A fallback tenant here would put every unscoped
// caller into one shared bucket that they could all read and write — the exact
// behaviour board-resolutions-svc had, where a missing header quietly became a
// tenant literally named "default".
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext puts X-Tenant-Id on the context when it is present, and does
// nothing when it is not. Refusing the request is the handler's job, so that
// the refusal is a considered 401 rather than a middleware-shaped surprise.
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
