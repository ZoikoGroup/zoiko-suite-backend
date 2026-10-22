package handler

import (
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/health"
	svcmiddleware "zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

// NewRouter assembles the service's complete HTTP surface — the one cmd/server
// serves and the error-contract tests exercise, so they cannot drift apart.
//
// /healthz and /readyz stay OUTSIDE the tenant gate: liveness and readiness
// probes carry no tenant, like every other service in this platform. Every
// registry route sits behind TenantContext and then runs the handler
// pre-flight (principal, tenant, authorization, ...). Transport-level errors
// (unknown route, unsupported method, recovered panic, failed readiness) use
// the same {"error","error_code"} envelope as handler errors.
func NewRouter(h *Handler, log *zap.Logger, pool *pgxpool.Pool) chi.Router {
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(svcmiddleware.Recoverer(log))

	r.Get("/healthz", health.HealthzHandler)
	r.Get("/readyz", health.ReadyzHandler(pool))

	r.Group(func(r chi.Router) {
		r.Use(svcmiddleware.TenantContext())
		RegisterRoutes(r, h)
	})

	svcmiddleware.JSONErrors(r)
	return r
}
