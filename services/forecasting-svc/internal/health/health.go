// Package health serves the public liveness and readiness probes.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Response is the probe body.
type Response struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Reason  string `json:"reason,omitempty"`
}

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// LivenessHandler handles GET /healthz: the process is up. It never touches
// dependencies, so a database outage does not get the pod restarted.
func LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeProbe(w, http.StatusOK, Response{Status: "ok", Service: "forecasting-svc"})
	}
}

// ReadinessHandler handles GET /readyz: 200 when the database answers a ping
// within two seconds, 503 otherwise (so the instance is taken out of load
// balancing until Postgres is reachable).
func ReadinessHandler(database Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if database == nil {
			writeProbe(w, http.StatusServiceUnavailable, Response{Status: "unavailable", Service: "forecasting-svc", Reason: "database pool not initialised"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.Ping(ctx); err != nil {
			writeProbe(w, http.StatusServiceUnavailable, Response{Status: "unavailable", Service: "forecasting-svc", Reason: "database ping failed"})
			return
		}
		writeProbe(w, http.StatusOK, Response{Status: "ready", Service: "forecasting-svc"})
	}
}

func writeProbe(w http.ResponseWriter, status int, body Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
