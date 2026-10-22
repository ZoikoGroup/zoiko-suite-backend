// Package middleware provides HTTP middleware for privacy-rights-svc.
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"zoiko.io/privacy-rights-svc/internal/domain"
)

type (
	tenantCtxKey      struct{}
	correlationCtxKey struct{}
)

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// WithCorrelation / CorrelationFromContext carry the caller's X-Correlation-ID
// to the authorization client, so the authorization-svc call is traceable to
// the request that caused it. Empty when the caller sent none (reads).
func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

func CorrelationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

// TenantContext is the transport-boundary gate of every route in the group it
// wraps. It runs BEFORE the handler, so it performs the first two steps of the
// documented pre-flight itself and in the documented order:
//
//  1. a non-empty X-Principal-Id   -> 401 AUTHENTICATION_FAILED
//  2. a non-empty, non-whitespace X-Tenant-Id -> 401 TENANT_SCOPE_REQUIRED
//
// so a request with neither header is told it is unauthenticated (principal
// first), exactly as the handlers' own requirePrincipal/requireTenant would. The
// handlers repeat both checks; this middleware only guarantees that no handler
// ever runs without a tenant.
//
// §18.1 requires tenant scope on all mutating APIs, and the verified tenant is
// the sole input to the transaction-local app.tenant_id used by the Row-Level
// Security policies (see internal/store.withTenant). A missing, empty or
// whitespace-only header would otherwise degrade to an empty scope rather than
// fail closed, so it is rejected here — before business logic, database work or
// idempotency persistence.
//
// This is transport-boundary validation only; it does not authenticate the
// principal or the tenant (that remains the platform's job) and does not change
// the RLS model. It also copies X-Correlation-ID, when present, into the
// request context for the authorization client.
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.TrimSpace(r.Header.Get("X-Principal-Id")) == "" {
				writeRefusal(w, domain.ErrCodeAuthenticationFailed, "X-Principal-Id header is required")
				return
			}
			tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
			if tenantID == "" {
				writeRefusal(w, domain.ErrCodeTenantScopeRequired, "X-Tenant-Id header is required")
				return
			}
			ctx := WithTenant(r.Context(), tenantID)
			if c := strings.TrimSpace(r.Header.Get("X-Correlation-ID")); c != "" {
				ctx = WithCorrelation(ctx, c)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeRefusal writes the service's standard 401 error envelope.
func writeRefusal(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "error_code": code})
}
