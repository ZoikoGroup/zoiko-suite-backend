// Package middleware provides HTTP middleware for tenant-entity-registry-svc.
package middleware

import (
	"net/http"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// Gateway-set identity headers. Traefik's ForwardAuth middleware verifies the
// caller's IdentityContextEnvelope against identity-svc's JWKS and copies the
// resolved identity into these headers; a request with a bad token never
// reaches this process at all. See the gateway-auth middleware definition in
// deployments/docker-compose.yml.
const (
	HeaderPrincipalID   = "X-Principal-Id"
	HeaderTenantID      = "X-Tenant-Id"
	HeaderLegalEntityID = "X-Legal-Entity-Id"
)

// Identity extracts the caller's verified identity into the request context.
//
// Until 2026-08-05 this middleware read tenant_id by base64-decoding the JWT
// payload from the Authorization header, with no signature check, on the
// stated assumption that "the Authorization Service has already validated the
// token upstream". Two things made that assumption false:
//
//   - The authorization client was a TODO that returned nil, so nothing
//     validated anything.
//   - docker-compose publishes 8081 on the host, so the gateway that does the
//     verifying can be bypassed by connecting to the service directly.
//
// tenant_id is what PgStore.withRLS sets as app.tenant_id on every Postgres
// session, so an unsigned, caller-supplied claim was steering row-level
// security — this service's central architectural guarantee. Forging
// {"tenant_id":"<victim>"} needs no key, just base64.
//
// Reading the gateway-set headers instead means the value is only ever one a
// verified envelope produced. A caller who sets the header directly is not
// trusted either: in production the gateway strips inbound copies of these
// headers before ForwardAuth, and the service is not published outside the
// cluster.
func Identity(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			if tenantID := r.Header.Get(HeaderTenantID); tenantID != "" {
				ctx = domain.WithTenant(ctx, tenantID)
			} else {
				// Not an error: ProvisionTenant is the bootstrap call and has
				// no tenant yet — it sets the context from the new ID itself.
				log.Debug("no verified tenant on request; RLS relies on the store-level fallback",
					zap.String("path", r.URL.Path),
					zap.String("request_id", chimiddleware.GetReqID(ctx)),
				)
			}

			if principalID := r.Header.Get(HeaderPrincipalID); principalID != "" {
				ctx = domain.WithPrincipal(ctx, principalID)
			}

			if legalEntityID := r.Header.Get(HeaderLegalEntityID); legalEntityID != "" {
				ctx = domain.WithLegalEntity(ctx, legalEntityID)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
