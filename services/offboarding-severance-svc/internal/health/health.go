package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Response struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Service   string    `json:"service"`
}

func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(Response{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
		Service:   "offboarding-severance-svc",
	})
}

func ReadyzHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if pool != nil {
			if err := pool.Ping(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(Response{
					Status:    "unready",
					Timestamp: time.Now().UTC(),
					Service:   "offboarding-severance-svc",
				})
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{
			Status:    "ok",
			Timestamp: time.Now().UTC(),
			Service:   "offboarding-severance-svc",
		})
	}
}
