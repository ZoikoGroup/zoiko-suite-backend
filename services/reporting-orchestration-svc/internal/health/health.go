// Package health serves the liveness and readiness probes.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ServiceName is reported by both probes.
const ServiceName = "reporting-orchestration-svc"

// Pinger is anything whose reachability decides readiness (the DB pool).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler is /healthz: the process is alive. It checks no dependency.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok", "service": ServiceName})
	}
}

// ReadyHandler is /readyz: 200 only when the database answers a ping within
// two seconds, 503 otherwise.
func ReadyHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "service": ServiceName, "reason": "db pool not initialised"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "service": ServiceName, "reason": "db ping failed"})
			return
		}
		write(w, http.StatusOK, map[string]string{"status": "ready", "service": ServiceName})
	}
}

func write(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
