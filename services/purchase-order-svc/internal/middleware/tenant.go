// Package middleware provides HTTP middleware for purchase-order-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}
type correlationCtxKey struct{}

// WithTenant returns a context carrying tenantID for RLS enforcement.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the tenant_id set by TenantContext, or "" if absent.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// WithCorrelationID returns a context carrying the request's correlation id,
// so events written inside a store transaction carry the same id as the request
// that caused them.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

// CorrelationIDFromContext returns the correlation id set by TenantContext, or
// "" if the request carried none.
func CorrelationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

// TenantContext reads the caller's tenant scope from X-Tenant-Id — set by
// gateway-auth-svc's ForwardAuth verification (or Traefik, in a real
// deployment) after checking the signed IdentityContextEnvelope JWT, exactly
// like X-Principal-Id (see internal/handler.requirePrincipal). This service
// never decodes a JWT itself: identity and tenant scope are resolved once,
// upstream of every backend, not re-derived independently by each service
// (03-microservices.md §9.1 critical constraint). It also carries
// X-Correlation-ID into the context.
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				ctx = WithTenant(ctx, tenantID)
			}
			if correlationID := r.Header.Get("X-Correlation-ID"); correlationID != "" {
				ctx = WithCorrelationID(ctx, correlationID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
