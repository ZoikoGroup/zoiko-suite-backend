// Package router assembles the HTTP surface in one place so cmd/server and
// the route-contract tests exercise exactly the same middleware chain.
package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/external-data-feed-svc/internal/envelope"
	"zoiko.io/external-data-feed-svc/internal/handler"
	"zoiko.io/external-data-feed-svc/internal/health"
	"zoiko.io/external-data-feed-svc/internal/idempotency"
	"zoiko.io/external-data-feed-svc/internal/middleware"
)

// Deps are the collaborators the router wires together.
type Deps struct {
	DB      health.Pinger
	Idem    idempotency.Store
	Handler *handler.Handler
	Logger  *zap.Logger
}

// New returns the service router. Order follows the house convention:
// RequestID, RealIP, Recoverer, tenant, envelope, idempotency, routes. The
// probes are exempted by both the tenant middleware and the envelope policy.
func New(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(middleware.TenantContext)
	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4).
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	r.Use(idempotency.Middleware(d.Idem, d.Logger))

	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(d.DB))
	handler.RegisterRoutes(r, d.Handler)
	return r
}
