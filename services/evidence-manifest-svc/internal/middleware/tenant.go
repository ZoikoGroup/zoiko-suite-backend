// Package middleware provides HTTP middleware for evidence-manifest-svc.
//
// This package is new. Before it, the service had no tenant plumbing of any
// kind: X-Tenant-Id was never read, the tenant came from the request body on
// the one route that mentioned it at all, and the two read routes had no
// tenant input whatsoever — a manifest id from the URL was the entire
// argument. See TenantContext for why the refusal lives here rather than in
// each handler.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

// WithTenant returns a context carrying the caller's verified tenant.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the verified tenant, or "" when the request
// carried no X-Tenant-Id.
//
// It never substitutes a fabricated default. "" matches no tenant_id, so a
// tenant-scoped query made without one sees and writes nothing — the
// opposite of the "default-tenant" fallback found across the connector
// services, where a header-less request succeeded into a shared synthetic
// bucket.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext requires a gateway-verified X-Tenant-Id on every request and
// refuses those without one.
//
// Unlike ai-governance-svc and commercial-account-svc, this service gets a
// blanket refusal rather than a per-handler check, because it has no
// platform-scope routes: all three of its endpoints operate on one tenant's
// evidence. There is no catalog or taxonomy here that every tenant reads in
// common, so nothing legitimately arrives without a tenant.
//
// This is the whole of the service's request-level access control at
// present. It has no authorization client at all — no internal/authz
// package, no CheckAllowed call on any route — so within a tenant every
// principal can read and generate every manifest. That gap is tracked
// separately; it needs its own action constants and an authz client, and is
// not something a tenant-isolation change should invent quietly.
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := r.Header.Get("X-Tenant-Id")
			if tenantID == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"missing_tenant_scope",` +
					`"message":"X-Tenant-Id is required — the gateway sets it from a verified identity envelope"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithTenant(r.Context(), tenantID)))
		})
	}
}
