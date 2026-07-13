// Package health provides liveness and readiness probes for
// configuration-feature-flag-svc.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Handler serves /healthz and /readyz probes.
type Handler struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New constructs a health Handler.
func New(pool *pgxpool.Pool, log *zap.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// Liveness handles GET /healthz. Always returns 200 if the process is alive.
func (h *Handler) Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Readiness handles GET /readyz.
// Returns 200 only if the DB pool can be pinged within 2 seconds.
// Returns 503 if the pool is unavailable — orchestrators will hold traffic.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.pool.Ping(ctx); err != nil {
		h.log.Error("readiness probe: db ping failed", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "reason": "db_unreachable"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
