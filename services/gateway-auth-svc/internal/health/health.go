// Package health provides liveness/readiness probes for gateway-auth-svc.
package health

import (
	"context"
	"net/http"
	"time"

	"zoiko.io/gateway-auth-svc/internal/jwks"
)

// Liveness always returns 200 — this service holds no connections that can
// go unhealthy on their own; the process being up is enough.
func Liveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// Readiness confirms identity-context-svc's JWKS endpoint is actually
// reachable, since every request through the gateway depends on it.
func Readiness(c *jwks.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
