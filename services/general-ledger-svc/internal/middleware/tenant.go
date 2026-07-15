// Package middleware provides HTTP middleware for general-ledger-svc.
package middleware

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
)

type tenantCtxKey struct{}

// WithTenant returns a context carrying tenantID for RLS enforcement.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the tenant_id set by TenantContext, or "" if absent.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext extracts the tenant_id claim from the IdentityContextEnvelope
// JWT in the Authorization header and injects it into the request context.
// Every DB transaction in PgStore reads this to set app.tenant_id on the
// Postgres session, enforcing RLS. Mirrors tenant-entity-registry-svc's
// middleware of the same name exactly.
func TenantContext(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := extractTenantIDFromBearer(r.Header.Get("Authorization"))
			if tenantID != "" {
				r = r.WithContext(WithTenant(r.Context(), tenantID))
			} else {
				log.Debug("tenant_id not present in JWT; RLS will rely on store-level fallback",
					zap.String("path", r.URL.Path),
					zap.String("request_id", chimiddleware.GetReqID(r.Context())),
				)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractTenantIDFromBearer(authHeader string) string {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return ""
	}
	return jwtClaimString(token, "tenant_id")
}

func jwtClaimString(token, claim string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	v, ok := claims[claim]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
