// Package middleware provides HTTP middleware for tenant-entity-registry-svc.
package middleware

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// TenantContext extracts the tenant_id claim from the IdentityContextEnvelope
// JWT in the Authorization header and injects it into the request context via
// domain.WithTenant. Every DB transaction in PgStore calls withRLS which reads
// this value to set app.tenant_id on the Postgres session, enforcing RLS.
//
// Requests that carry no tenant_id (e.g. the ProvisionTenant bootstrap call)
// are permitted through — CreateTenant sets the context from the new ID itself.
// All other mutating store calls require this middleware to have set it first.
func TenantContext(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := extractTenantIDFromBearer(r.Header.Get("Authorization"))
			if tenantID != "" {
				r = r.WithContext(domain.WithTenant(r.Context(), tenantID))
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

// extractTenantIDFromBearer parses the Bearer JWT and extracts the tenant_id
// claim from the payload. Signature verification is not performed here — the
// Authorization Service has already validated the token upstream.
// The IdentityContextEnvelope carries tenant_id as a top-level JWT claim.
func extractTenantIDFromBearer(authHeader string) string {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return ""
	}
	return jwtClaimString(token, "tenant_id")
}

// jwtClaimString decodes the JWT payload and returns the string value of the
// named claim. Returns "" if the token is malformed or the claim is absent.
func jwtClaimString(token, claim string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}

	// JWT uses base64url (RFC 4648 §5) without padding.
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	// Unmarshal only into a map to avoid coupling to a specific claims struct.
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
