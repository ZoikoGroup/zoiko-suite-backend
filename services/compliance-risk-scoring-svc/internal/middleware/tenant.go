package middleware

import (
	"context"
	"net/http"
	"strings"

	"zoiko.io/compliance-risk-scoring-svc/internal/problem"
)

type contextKey string

// TenantIDKey is the request-context key holding the verified tenant.
const TenantIDKey contextKey = "tenant_id"

// maxTenantIDLength matches risk_score_assessments.tenant_id VARCHAR(64).
const maxTenantIDLength = 64

// TenantMiddleware records the gateway-verified X-Tenant-Id on the request
// context and REFUSES a request that carries none (401 problem+json) or one
// longer than the column that stores it (400).
//
// History: this middleware once substituted the literal "default-tenant"
// when the header was absent. Because the store pushes the tenant into
// Postgres (set_config('app.tenant_id', ...)) and the RLS policy reads it
// back, the fabricated value did not bypass RLS — it SATISFIED it, pooling
// every header-less caller into one shared tenant. The tenant is now never
// invented: no header, no request.
//
// The envelope middleware already refuses tenant-less WRITES; this gate is
// what refuses tenant-less READS, which write-strict envelope mode admits.
func TenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness/readiness probes carry no tenant; they touch no tenant
		// data, so they are the only paths admitted without one.
		switch r.URL.Path {
		case "/healthz", "/readyz", "/health":
			next.ServeHTTP(w, r)
			return
		}
		tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
		if tenantID == "" {
			problem.Write(w, r, http.StatusUnauthorized, "missing_tenant_scope",
				"X-Tenant-Id is required; the gateway sets it from a verified identity envelope", nil)
			return
		}
		if len(tenantID) > maxTenantIDLength {
			problem.Write(w, r, http.StatusBadRequest, "invalid_tenant_id", "X-Tenant-Id must be at most 64 characters", nil)
			return
		}

		ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithTenant returns a context carrying tenantID (tests).
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// GetTenantID returns the verified tenant, or "" when there is none. It
// never substitutes a placeholder.
func GetTenantID(ctx context.Context) string {
	if value, ok := ctx.Value(TenantIDKey).(string); ok {
		return value
	}
	return ""
}
