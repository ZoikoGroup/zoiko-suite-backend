// Package health provides the liveness and readiness probe handler.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type status struct {
	Status     string            `json:"status"`
	Checks     map[string]string `json:"checks"`
	CheckedAt  time.Time         `json:"checked_at"`
}

// Handler returns HTTP 200 when all critical dependencies are reachable,
// HTTP 503 otherwise (per 03-microservices.md §3.8 observability requirement).
type Handler struct {
	rdb  *redis.Client
	pool *pgxpool.Pool
}

func NewHandler(rdb *redis.Client, pool *pgxpool.Pool) *Handler {
	return &Handler{rdb: rdb, pool: pool}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	healthy := true

	// Redis check
	if err := h.rdb.Ping(r.Context()).Err(); err != nil {
		checks["redis"] = "unreachable: " + err.Error()
		healthy = false
	} else {
		checks["redis"] = "ok"
	}

	// Postgres check
	pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.pool.Ping(pingCtx); err != nil {
		checks["postgres"] = "unreachable: " + err.Error()
		healthy = false
	} else {
		checks["postgres"] = "ok"
	}

	// TODO: add upstream Tenant Registry liveness check

	s := status{
		Checks:    checks,
		CheckedAt: time.Now().UTC(),
	}
	if healthy {
		s.Status = "healthy"
		w.WriteHeader(http.StatusOK)
	} else {
		s.Status = "degraded"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}
