// Package router assembles the HTTP surface in one place so cmd/server and
// the route-contract tests exercise exactly the same middleware chain.
package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/tax-authority-interface-svc/internal/envelope"
	"zoiko.io/tax-authority-interface-svc/internal/handler"
	"zoiko.io/tax-authority-interface-svc/internal/health"
	"zoiko.io/tax-authority-interface-svc/internal/idempotency"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

// Deps are the collaborators the router wires together.
type Deps struct {
	DB      health.Pinger
	Idem    idempotency.Store
	Handler *handler.Handler
	Logger  *zap.Logger
}

// New returns the service router.
//
// The probes and the authority callback receiver sit on the root router: the
// probes carry no identity, and the callback is machine-authenticated by its
// signature (verified against the tenant+provider secret in the URL), not by
// a tenant header. Everything else runs tenant, envelope, idempotency.
func New(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)

	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(d.DB))
	handler.RegisterCallbackRoute(r, d.Handler)

	r.Group(func(r chi.Router) {
		r.Use(middleware.TenantContext)
		// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4).
		// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
		r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
		// Idempotency-Key replay: a retried material write is answered from its first response.
		r.Use(idempotency.Middleware(d.Idem, d.Logger))
		handler.RegisterRoutes(r, d.Handler)
	})
	return r
}
