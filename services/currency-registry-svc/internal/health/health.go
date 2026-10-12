package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Pinger is the one method readiness needs from the database pool.
//
// Narrowed to an interface so the probe can be tested without a live Postgres.
// It is not only convenience: *pgxpool.Pool panics on Ping when it is nil, so a
// probe written against the concrete type turns "the pool was never built" into
// a crash inside the health endpoint — the one handler that must survive
// everything else being broken.
type Pinger interface {
	Ping(context.Context) error
}

// Dependency is one thing readiness asks about besides the database.
type Dependency struct {
	Name  string
	Check func(context.Context) error
}

type Health struct {
	pool Pinger
	log  *zap.Logger
	deps []Dependency
}

func New(pool Pinger, log *zap.Logger, deps ...Dependency) *Health {
	return &Health{pool: pool, log: log, deps: deps}
}

// Liveness answers whenever the process is up. It deliberately checks nothing:
// a liveness probe that fails on a dependency outage gets the container killed
// and restarted, which fixes nothing and removes the one instance that could
// have reported what was wrong.
func (h *Health) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// Readiness reports whether this instance can actually serve a request.
//
// The database is not the only answer, and treating it as one was a real gap
// here. EVERY endpoint on this service — including both reads — calls
// authorization-svc before it does anything, and this service fails closed, so
// with authorization-svc unreachable a healthy database means every single
// request returns 503 while the container reports ready and the load balancer
// keeps sending traffic. A probe that cannot distinguish "serving" from
// "answering 503 promptly" is not a readiness probe.
//
// The body names which component failed, because the two have opposite
// remedies: a dead pool is this service's problem, an unreachable
// authorization-svc is somebody else's and restarting this one will not help.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	components := map[string]string{}
	ready := true

	if h.pool == nil {
		components["database"] = "no pool configured"
		ready = false
	} else if err := h.pool.Ping(ctx); err != nil {
		h.log.Error("readiness check failed: db ping error", zap.Error(err))
		components["database"] = "unreachable: " + err.Error()
		ready = false
	} else {
		components["database"] = "ok"
	}

	for _, d := range h.deps {
		if err := d.Check(ctx); err != nil {
			h.log.Error("readiness check failed", zap.String("dependency", d.Name), zap.Error(err))
			components[d.Name] = "unreachable: " + err.Error()
			ready = false
			continue
		}
		components[d.Name] = "ok"
	}

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     map[bool]string{true: "READY", false: "NOT_READY"}[ready],
		"components": components,
	})
}
