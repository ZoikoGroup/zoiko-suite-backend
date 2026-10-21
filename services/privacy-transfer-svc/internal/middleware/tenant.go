// Package middleware provides HTTP middleware for privacy-transfer-svc.
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type tenantCtxKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext enforces a non-empty, non-whitespace X-Tenant-Id on every
// request in the group it wraps, before any handler runs.
//
// §18.1 requires tenant scope on all mutating APIs, and the verified tenant
// is the sole input to the transaction-local app.tenant_id used by the
// Row-Level Security policies (see internal/store.withTenant). A missing,
// empty or whitespace-only header would otherwise degrade to an empty
// scope (tenant_id IS NULL platform rows) rather than fail closed, so it is
// rejected here — before business logic, database work, idempotency
// persistence or event publication.
//
// This is transport-boundary validation only; it does not authenticate the
// tenant (that remains the platform's job) and does not change the RLS
// model.
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
			if tenantID == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "X-Tenant-Id header is required"})
				return
			}
			next.ServeHTTP(w, r.WithContext(WithTenant(r.Context(), tenantID)))
		})
	}
}
