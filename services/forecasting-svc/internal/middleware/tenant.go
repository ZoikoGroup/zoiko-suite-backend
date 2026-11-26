// Package middleware holds the tenant gate.
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type contextKey string

// TenantIDKey is the context key for the verified tenant.
const TenantIDKey contextKey = "tenant_id"

// TenantMiddleware records the gateway-verified X-Tenant-Id on the request
// context and REFUSES (401) a request that carries none.
//
// The tenant is never fabricated. An earlier version substituted
// "default-tenant" when the header was missing; because the store pushes the
// tenant into Postgres (set_config('app.tenant_id', ...)) and RLS compares
// against it, that default made every header-less caller share one tenant's
// rows *inside* the policy. Refusing is the only safe behaviour.
//
// It runs for reads as well as writes: the envelope middleware in
// write-strict mode only refuses writes, so this is what stops a GET without a
// tenant.
func TenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes carry no identity and touch no tenant data.
		switch r.URL.Path {
		case "/healthz", "/readyz", "/health":
			next.ServeHTTP(w, r)
			return
		}
		tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
		if tenantID == "" {
			writeMissingTenant(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeMissingTenant(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type":           "https://api.zoikosuite.com/problems/authentication-required",
		"title":          "Authentication is required.",
		"status":         http.StatusUnauthorized,
		"detail":         "X-Tenant-Id is required; the gateway sets it from a verified identity envelope",
		"instance":       r.URL.Path,
		"code":           "missing_tenant_scope",
		"request_id":     r.Header.Get("X-Request-Id"),
		"correlation_id": r.Header.Get("X-Correlation-ID"),
		"retryable":      false,
	})
}

// WithTenant returns a context carrying tenantID (tests).
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// GetTenantID returns the verified tenant, or "" when there is none. It never
// substitutes a placeholder.
func GetTenantID(ctx context.Context) string {
	if val, ok := ctx.Value(TenantIDKey).(string); ok {
		return val
	}
	return ""
}
