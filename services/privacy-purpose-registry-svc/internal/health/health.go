package health

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func ReadyzHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			middleware.WriteError(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "database pool not initialised")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			middleware.WriteError(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "database ping failed")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}
