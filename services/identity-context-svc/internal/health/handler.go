// Package health provides the liveness and readiness probe handler.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type status struct {
	Status    string            `json:"status"`
	Checks    map[string]string `json:"checks"`
	CheckedAt time.Time         `json:"checked_at"`
}

// OutboxProbe reports the outbox's depth.
//
// An interface rather than *outbox.Relay so this package does not depend on
// the outbox, which would be a cycle waiting to happen the first time the
// relay wants to report its own health.
type OutboxProbe interface {
	PendingCount(ctx context.Context) (int, error)
	DeadLetterCount(ctx context.Context) (int, error)
}

// UpstreamProbe is a named upstream this service fails closed on.
type UpstreamProbe interface {
	// Name is what appears in the checks map.
	Name() string
	// Ping reports whether the upstream is answering.
	Ping(ctx context.Context) error
}

// Handler returns HTTP 200 when all critical dependencies are reachable,
// HTTP 503 otherwise (per 03-microservices.md §3.8 observability requirement).
type Handler struct {
	rdb       *redis.Client
	pool      *pgxpool.Pool
	outbox    OutboxProbe
	upstreams []UpstreamProbe

	// outboxBacklogThreshold is where a growing outbox stops being normal and
	// starts being a degraded service.
	//
	// A backlog is NOT unhealthy on its own — that is the whole point of the
	// outbox, and a broker restart should not take this service out of the
	// load balancer. What IS unhealthy is a backlog large enough that the
	// relay has clearly stopped rather than merely fallen behind.
	outboxBacklogThreshold int
}

func NewHandler(rdb *redis.Client, pool *pgxpool.Pool) *Handler {
	return &Handler{rdb: rdb, pool: pool, outboxBacklogThreshold: 10000}
}

// WithOutbox adds the outbox depth to the probe.
//
// This is the one number worth alerting on in the new architecture: everything
// else about this service can look perfectly healthy while events pile up
// unseen, because the relay failing is invisible from the request path.
func (h *Handler) WithOutbox(p OutboxProbe, backlogThreshold int) *Handler {
	h.outbox = p
	if backlogThreshold > 0 {
		h.outboxBacklogThreshold = backlogThreshold
	}
	return h
}

// WithUpstream adds a Tier 0 dependency to the readiness check.
func (h *Handler) WithUpstream(p UpstreamProbe) *Handler {
	h.upstreams = append(h.upstreams, p)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	healthy := true

	// Redis check.
	//
	// The nil guard is not defensive padding: it is what lets the probe's own
	// branching — backlog thresholds, upstream aggregation, which conditions
	// degrade and which merely report — be tested without a live Redis and
	// Postgres. Both are always set in cmd/server, and a deployment that
	// somehow reached here with neither would show an empty checks map rather
	// than a false "healthy".
	if h.rdb != nil {
		if err := h.rdb.Ping(r.Context()).Err(); err != nil {
			checks["redis"] = "unreachable: " + err.Error()
			healthy = false
		} else {
			checks["redis"] = "ok"
		}
	}

	// Postgres check
	if h.pool != nil {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := h.pool.Ping(pingCtx); err != nil {
			checks["postgres"] = "unreachable: " + err.Error()
			healthy = false
		} else {
			checks["postgres"] = "ok"
		}
		cancel()
	}

	// Outbox depth.
	//
	// Reported ALWAYS, and only unhealthy past the threshold. A pending count
	// of a few hundred during a broker blip is the outbox doing its job; a
	// count in the tens of thousands means the relay is not running and every
	// governance event this service has produced is sitting in a table.
	if h.outbox != nil {
		obCtx, obCancel := context.WithTimeout(r.Context(), 2*time.Second)
		pending, err := h.outbox.PendingCount(obCtx)
		if err != nil {
			checks["outbox"] = "unreadable: " + err.Error()
			// Not fatal on its own — Postgres is already checked above, and a
			// failure here with a healthy Postgres is a query problem, not an
			// availability one.
		} else {
			checks["outbox_pending"] = strconv.Itoa(pending)
			if pending > h.outboxBacklogThreshold {
				checks["outbox"] = "backlog exceeds threshold — relay may not be running"
				healthy = false
			} else {
				checks["outbox"] = "ok"
			}
		}

		// Dead letters never make the service unhealthy: they are a backlog of
		// events nobody can deliver, which needs an operator, not a restart.
		// Taking the pod out of rotation would not fix one of them.
		if dead, err := h.outbox.DeadLetterCount(obCtx); err == nil && dead > 0 {
			checks["outbox_dead_letter"] = strconv.Itoa(dead)
		}
		obCancel()
	}

	for _, up := range h.upstreams {
		upCtx, upCancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := up.Ping(upCtx); err != nil {
			checks[up.Name()] = "unreachable: " + err.Error()
			healthy = false
		} else {
			checks[up.Name()] = "ok"
		}
		upCancel()
	}

	s := status{
		Checks:    checks,
		CheckedAt: time.Now().UTC(),
	}
	// Content-Type before WriteHeader — setting a header after the status line
	// has been written is a no-op, so the previous order silently served these
	// as text/plain.
	w.Header().Set("Content-Type", "application/json")
	if healthy {
		s.Status = "healthy"
		w.WriteHeader(http.StatusOK)
	} else {
		s.Status = "degraded"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	_ = json.NewEncoder(w).Encode(s)
}
