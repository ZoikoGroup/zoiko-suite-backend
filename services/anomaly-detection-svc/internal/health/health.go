// Package health serves the liveness and readiness probes. Both are
// registered on the root router and exempted from the tenant middleware and
// the envelope policy, so they answer without identity headers.
package health

import (
	"context"
	"net/http"
	"time"
)

// Pinger is the one capability /readyz needs from the database pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthzHandler is the liveness probe: the process is up. No dependencies.
func HealthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ReadyzHandler is the readiness probe: 200 only when the database answers a
// ping within 2s, 503 otherwise.
func ReadyzHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if db == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","reason":"db pool not initialised"}`))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","reason":"db ping failed"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}
