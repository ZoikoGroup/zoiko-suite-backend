package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	svcenvelope "zoiko.io/anomaly-detection-svc/internal/envelope"
	"zoiko.io/anomaly-detection-svc/internal/health"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
)

// NewRouter builds the service's full HTTP stack, in the platform order:
// RequestID, RealIP, Logger, Recoverer, tenant gate, §4 envelope, routes.
// The probes /healthz and /readyz are exempted by both the tenant gate and
// the envelope policy, so they answer without identity headers. readiness
// may be nil (readyz then reports 503).
func NewRouter(h *Handler, readiness health.Pinger) http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4): no request
	// reaches business logic without a resolved tenant, actor, correlation
	// and — on material writes — an idempotency key and legal entity.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.NotFound(NotFoundHandler)
	r.MethodNotAllowed(MethodNotAllowedHandler)

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(readiness))
	RegisterRoutes(r, h)
	return r
}
