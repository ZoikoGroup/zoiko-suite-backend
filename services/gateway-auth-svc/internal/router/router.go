// Package router builds gateway-auth-svc's HTTP surface.
//
// It exists as its own package so the wiring is testable. Every existing test
// in internal/handler calls Handler.Verify directly, which is why the middleware
// stack around it went unexercised — and why a middleware that refused /verify
// on any non-GET method could sit in main.go without a single test failing.
// Anything mounted here is reachable from a test exactly as Traefik reaches it.
package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"zoiko.io/gateway-auth-svc/internal/envelope"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/health"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/telemetry"
)

// New assembles the router main.go serves and tests exercise.
//
// metrics may be nil, which leaves the HTTP metrics middleware and /metrics
// off. Every existing test calls New with nil, so none of them needs a
// Prometheus registry — and two registrations of the same collector inside one
// test binary would panic.
func New(h *handler.Handler, jwksClient *jwks.Client, metrics *telemetry.Metrics) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Ahead of the envelope middleware so a refusal is still counted. A
	// contract violation is a real outcome of a real request; leaving it out
	// of http_requests_total would make the one metric that shows total load
	// disagree with the gateway's own logs.
	if metrics != nil {
		r.Use(metrics.HTTPMiddleware)
	}

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// /verify is exempt, declared in the generated ServicePolicy. It is the
	// endpoint that establishes tenant and actor from the signed token, so it
	// cannot also demand them as input — see EXEMPT_PATHS in rollout.sh.
	r.Use(envelope.Middleware(envelope.ServicePolicy(), envelope.DefaultReporter()))

	r.Get("/healthz", health.Liveness)
	if metrics != nil {
		// WrapReadiness keeps the readiness_up gauge in step with what /readyz
		// actually answered, rather than with a separate probe that could
		// disagree with it.
		r.Get("/readyz", metrics.WrapReadiness(health.Readiness(jwksClient)))
		r.Handle("/metrics", metrics.MetricsHandler(health.Readiness(jwksClient), promhttp.Handler()))
	} else {
		r.Get("/readyz", health.Readiness(jwksClient))
	}

	// Traefik's ForwardAuth middleware calls this with the incoming
	// request's original method, so it must not be restricted to GET.
	r.Handle("/verify", http.HandlerFunc(h.Verify))

	return r
}
