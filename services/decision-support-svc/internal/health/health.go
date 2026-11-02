// Package health serves the liveness and readiness probes. Both are
// registered on the root router and are exempt from the tenant and envelope
// checks, so an orchestrator can call them without identity headers.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Pinger is the subset of *pgxpool.Pool the readiness probe needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Health struct {
	db  Pinger
	log *zap.Logger
}

// New builds the probes on pool. A nil pool makes readiness report 503.
func New(pool *pgxpool.Pool, log *zap.Logger) *Health {
	if pool == nil {
		// Avoid a typed-nil interface: Readiness must see a nil Pinger.
		return NewWithPinger(nil, log)
	}
	return NewWithPinger(pool, log)
}

// NewWithPinger builds the probes on any Pinger (tests).
func NewWithPinger(db Pinger, log *zap.Logger) *Health {
	if log == nil {
		log = zap.NewNop()
	}
	return &Health{db: db, log: log}
}

// Liveness reports that the process is up. It checks no dependency.
func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, `{"status":"ok"}`)
}

// Readiness pings the database with a 2s timeout: 200 when it answers,
// 503 otherwise.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeStatus(w, http.StatusServiceUnavailable, `{"status":"unavailable","reason":"db pool not initialised"}`)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.log.Error("readiness check failed: db ping error", zap.Error(err))
		writeStatus(w, http.StatusServiceUnavailable, `{"status":"unavailable","reason":"db ping failed"}`)
		return
	}

	writeStatus(w, http.StatusOK, `{"status":"ready"}`)
}

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
