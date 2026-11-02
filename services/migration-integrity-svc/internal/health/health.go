package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Handler is the liveness probe: the process is up. It checks no dependency.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": "migration-integrity-svc",
		})
	}
}

// Pinger is the subset of *pgxpool.Pool the readiness probe needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// ReadyzHandler is the readiness probe: 200 when the database answers a ping
// within 2s, 503 otherwise (including when no pool is configured).
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
