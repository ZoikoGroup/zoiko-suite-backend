// Package middleware provides HTTP middleware for ai-governance-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the verified tenant, or "" when the request
// carried no X-Tenant-Id. It never substitutes a fabricated default: ""
// matches no real tenant_id, so a tenant-scoped query made without one
// sees and writes nothing.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext records the gateway-verified X-Tenant-Id on the request
// context. It deliberately does NOT reject a request that lacks the
// header, unlike the equivalent middleware in the connector services.
//
// This service serves two kinds of route. Three of its six tables carry no
// tenant_id at all and are correct that way: action_risk_classifications
// (the risk taxonomy), model_provider_registrations (the provider
// registry), and policy_change_approvals — doc7 §G3 says policy changes
// "alter governance truth across tenants", so approving one is platform
// administration, not a tenant's own data. A blanket 401 here would break
// those routes.
//
// Enforcement therefore lives in the handlers that actually touch
// tenant-scoped data (ai_runs, automation_policies, automation_actions),
// via Handler.requireTenant. Putting it here instead would either break
// the platform routes or, if softened, leave the tenant routes unguarded.
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
