// Package health provides liveness and readiness probes for
// configuration-feature-flag-svc.
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

// Handler serves /healthz and /readyz probes.
type Handler struct {
	pool Pinger
	log  *zap.Logger
	deps []Dependency
}

// New constructs a health Handler. Extra dependencies are probed alongside the
// database and named individually in the response body.
func New(pool Pinger, log *zap.Logger, deps ...Dependency) *Handler {
	return &Handler{pool: pool, log: log, deps: deps}
}

// Liveness handles GET /healthz. It answers whenever the process is up and
// deliberately checks nothing: a liveness probe that fails on a dependency
// outage gets the container killed and restarted, which fixes nothing and
// removes the one instance that could have reported what was wrong.
func (h *Handler) Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Readiness handles GET /readyz and reports whether this instance can actually
// serve a request.
//
// The database is not the only answer, and treating it as one was a real gap
// here. EVERY write on this service calls authorization-svc first and FAILS
// CLOSED on the result: with authorization-svc unreachable, POST /v1/config and
// POST /v1/flags both answer 503 authz_unavailable while the pool is perfectly
// healthy. This probe used to ping only the pool, so the container reported
// ready, the orchestrator kept routing to it, and 100% of writes failed with
// nothing in the readiness signal to show for it — on a service whose reads all
// kept working, which is what made it look like a console bug.
//
// The body names which component failed, because the two have opposite
// remedies: a dead pool is this service's problem, an unreachable
// authorization-svc is somebody else's and restarting this one will not help.
//
// Reads are deliberately NOT a reason to stay ready on their own. Configuration
// stays readable with authorization-svc down, which is what keeps consumers
// running — but an instance that can only be read from cannot serve this
// service's purpose, which is recording changes.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
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
