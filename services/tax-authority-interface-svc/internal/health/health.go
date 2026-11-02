// Package health serves the liveness (/healthz) and readiness (/readyz)
// probes. Both are registered on the root router and exempted from the
// tenant middleware and the envelope policy, so an orchestrator can reach
// them without identity headers.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const serviceName = "tax-authority-interface-svc"

// Response is the probe body.
type Response struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Reason  string `json:"reason,omitempty"`
}

// Pinger is the part of *pgxpool.Pool readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler is liveness: the process is up and serving. It checks no
// dependency, so a database outage never gets the container restarted.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, Response{Status: "ok", Service: serviceName})
	}
}

// ReadyHandler is readiness: 200 only when the database answers a ping
// within 2s, otherwise 503 so traffic is withheld.
func ReadyHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			write(w, http.StatusServiceUnavailable, Response{Status: "unavailable", Service: serviceName, Reason: "db pool not initialised"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			write(w, http.StatusServiceUnavailable, Response{Status: "unavailable", Service: serviceName, Reason: "db ping failed"})
			return
		}
		write(w, http.StatusOK, Response{Status: "ready", Service: serviceName})
	}
}

func write(w http.ResponseWriter, status int, body Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
