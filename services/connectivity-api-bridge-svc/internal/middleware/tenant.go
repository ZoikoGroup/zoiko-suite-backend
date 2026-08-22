package middleware

import (
	"context"
	"encoding/json"
	"net/http"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

// TenantContext reads the caller's verified tenant scope from X-Tenant-Id
// (set by gateway-auth-svc's ForwardAuth verification) and refuses the
// request if it is absent.
//
// This used to substitute the literal string "default-tenant" when the
// header was missing. That did not merely skip a check — it fabricated an
// identity: a header-less request succeeded, attributed to a tenant that
// does not exist, and every header-less request from every caller landed
// in the SAME "default-tenant" bucket. Those rows were co-mingled, and any
// caller could read another's data by simply OMITTING the header, which is
// the easier request to make. That made the insecure path the path of
// least resistance — the same shape as document-vault-svc's
// filter-that-disables-itself and payroll-exceptions-svc's "GLOBAL"
// sentinel, and a direct violation of the platform's "never fabricate a
// signal with nothing real to populate it" doctrine.
//
// Note on the header name: Go canonicalises header keys, so Header.Get
// matches "X-Tenant-Id", "X-Tenant-ID" and any other casing. The spelling
// here is cosmetic, not functional.
func TenantContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "missing_tenant_scope",
				"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
			})
			return
		}
		ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithTenant returns a context carrying tenantID, for callers that resolve
// a tenant outside the HTTP path (and for tests). Named to match the same
// helper in workflow-svc, policy-svc and configuration-feature-flag-svc.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// GetTenantID returns the verified tenant from ctx, or "" if absent.
//
// It returns the empty string rather than a fabricated default for the
// same reason as above: a caller that reaches a query with no tenant must
// match no rows, never every row belonging to a synthetic tenant. This is
// the second half of the defect — leaving it would keep the hole open even
// with the middleware fixed.
func GetTenantID(ctx context.Context) string {
	if val, ok := ctx.Value(TenantIDKey).(string); ok {
		return val
	}
	return ""
}
