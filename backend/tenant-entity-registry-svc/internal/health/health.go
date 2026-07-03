// Package health provides liveness and readiness probes for the service.
//
// Liveness  — GET /healthz  — always returns 200 if the process is running.
// Readiness — GET /readyz   — returns 200 only if the DB pool is reachable.
//
// Per doctrine §3.8: no service is production-ready without health probes.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Handler exposes liveness and readiness probes.
type Handler struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New constructs a health Handler.
func New(pool *pgxpool.Pool, log *zap.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

type healthResponse struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Service   string    `json:"service"`
}

// Liveness — GET /healthz
// Always 200 while the process is alive. Kubernetes restarts on repeated failure.
func (h *Handler) Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
		Service:   "tenant-entity-registry-svc",
	})
}

// Readiness — GET /readyz
// Returns 200 only if the database pool is reachable.
// Kubernetes stops routing traffic if this probe fails.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.pool.Ping(ctx); err != nil {
		h.log.Error("readiness probe: db unreachable", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:    "db_unavailable",
			Timestamp: time.Now().UTC(),
			Service:   "tenant-entity-registry-svc",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
		Service:   "tenant-entity-registry-svc",
	})
}
