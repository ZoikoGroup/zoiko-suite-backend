// Package health serves the liveness and readiness probes.
//
// Both are public: the tenant check lives in the handlers (requireTenant) and
// the envelope policy exempts /healthz and /readyz, so a probe needs no
// identity headers.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger is the one thing readiness needs from the database pool.
// *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthzHandler reports that the process is alive. It checks no dependency:
// a liveness probe that fails when Postgres is down gets the container
// restarted for an outage it cannot fix.
func HealthzHandler(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": service})
	}
}

// ReadyzHandler reports ready only when the database answers within 2s. The
// records this service serves live in Postgres alone, so without it the
// service can answer nothing correctly and must be taken out of rotation.
// A nil pinger is not ready — there is no in-memory fallback in production.
func ReadyzHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "db pool not initialised"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "db ping failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
