// Package health serves the liveness (/healthz) and readiness (/readyz)
// probes. Both are registered on the root router and exempted from the
// tenant middleware and the envelope policy, because probes carry no
// identity headers.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const serviceName = "banking-connector-svc"

// Response is the probe body.
type Response struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Reason  string `json:"reason,omitempty"`
}

// Pinger is the readiness dependency; *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler is the liveness probe: the process is up. It checks no dependency.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, Response{Status: "ok", Service: serviceName})
	}
}

// ReadyHandler is the readiness probe: 200 only when the database answers a
// ping within 2s, otherwise 503 so the instance is taken out of rotation.
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
