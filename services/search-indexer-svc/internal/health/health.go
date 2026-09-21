// Package health provides /healthz and /readyz.
//
// The split is the usual one and the reason matters here more than it does in
// a CRUD service: /healthz says the process is alive, /readyz says it can
// actually serve a search. A search plane whose engine is unreachable answers
// every query with an error, and a readiness probe that only checked the
// process would keep that instance in rotation indefinitely.
//
// What readiness does NOT include: whether any scope has an active generation.
// A freshly deployed instance with nothing registered is correctly ready — it
// is working, there is simply nothing to search yet — and gating readiness on
// registered content would mean the service could never be deployed before its
// first contract was published.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Checker is one named dependency probe.
type Checker struct {
	Name  string
	Probe func(context.Context) error
	// Critical marks a dependency the service cannot serve without. A
	// non-critical failure is reported in the body and does not fail the
	// probe — the event publisher is the case that matters: losing it costs
	// observability, and taking the instance out of rotation over it would
	// turn a telemetry outage into a search outage.
	Critical bool
}

type Handler struct {
	checkers []Checker
	timeout  time.Duration

	mu    sync.RWMutex
	ready bool
}

func New(timeout time.Duration, checkers ...Checker) *Handler {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Handler{checkers: checkers, timeout: timeout}
}

// SetReady records whether background work has reached a serving state. It
// gates readiness alongside the dependency probes: an instance whose projector
// registry has never loaded cannot answer a search correctly, because it does
// not know which generation is live.
func (h *Handler) SetReady(v bool) {
	h.mu.Lock()
	h.ready = v
	h.mu.Unlock()
}

func (h *Handler) isReady() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ready
}

// Liveness answers 200 whenever the process is running.
//
// Deliberately checks nothing. A liveness probe that consulted the database
// would have the orchestrator restart every instance during a database
// failover — turning a recoverable dependency outage into a full restart
// storm at the worst possible moment.
func (h *Handler) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "healthy"})
}

// Readiness probes every dependency and reports per-check detail.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	checks := make(map[string]string, len(h.checkers))
	ok := h.isReady()
	if !ok {
		checks["bootstrap"] = "projector registry has not loaded"
	} else {
		checks["bootstrap"] = "ok"
	}

	for _, c := range h.checkers {
		if err := c.Probe(ctx); err != nil {
			checks[c.Name] = err.Error()
			if c.Critical {
				ok = false
			}
			continue
		}
		checks[c.Name] = "ok"
	}

	status := http.StatusOK
	state := "ready"
	if !ok {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": checks})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
