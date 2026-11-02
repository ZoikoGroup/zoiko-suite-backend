package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	svcenvelope "zoiko.io/migration-integrity-svc/internal/envelope"
	"zoiko.io/migration-integrity-svc/internal/health"
	"zoiko.io/migration-integrity-svc/internal/middleware"
)

// NewRouter builds the route tree. Request path through the service:
//
//	chi RequestID/RealIP/Logger/Recoverer
//	→ envelope.Middleware   (§4 input contract: tenant, principal, request and
//	                         correlation ids, source channel; Idempotency-Key and
//	                         X-Legal-Entity-Id on writes)
//	→ TenantMiddleware      (X-Tenant-Id onto the context; 401 when absent)
//	→ handler               (this package)
//	→ store.Store           (PgStore in production)
//
// NewRouter has no database behind /readyz, so readiness reports 503; the
// server uses NewRouterWithReadiness with the real pool.
func NewRouter(h *Handler) http.Handler {
	return NewRouterWithReadiness(h, health.ReadyzHandler(nil))
}

// NewRouterWithReadiness is NewRouter with the /readyz handler supplied
// (health.ReadyzHandler(pool) in production).
func NewRouterWithReadiness(h *Handler, readyz http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Enforcement
	// mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict: writes missing a
	// mandatory field are refused, reads are admitted and flagged).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// /healthz and /readyz stay OUTSIDE the tenant gate: probes carry no tenant.
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", readyz)

	// Everything below requires a gateway-verified tenant. Applied via With()
	// on the subtree rather than as a path exemption inside the middleware:
	// comparing r.URL.Path to skip an auth gate is a classic bypass source.
	r.With(middleware.TenantMiddleware).Route("/v1/migrations", func(r chi.Router) {
		r.Post("/validate", h.ValidateMigration)
		r.Get("/", h.ListJobs)
		r.Get("/{id}", h.GetJobByID)
		r.Post("/{id}/audit/{entryId}/remediate", h.RemediateEntry)
		r.Delete("/{id}", h.ArchiveJob)
	})

	return r
}
