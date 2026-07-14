// Package health provides the /healthz and /readyz HTTP handlers for
// search-indexer-svc. Follows the same pattern as obligations-svc and
// audit-event-store-svc (no external dependency required to report healthy).
package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

type statusResponse struct {
	Status string `json:"status"`
}

var (
	readyMu sync.RWMutex
	ready   = false // Start false until first successful sync cycle completes
)

// SetReady updates the readiness status of the service.
func SetReady(isReady bool) {
	readyMu.Lock()
	defer readyMu.Unlock()
	ready = isReady
}

// IsReady returns the current readiness status.
func IsReady() bool {
	readyMu.RLock()
	defer readyMu.RUnlock()
	return ready
}

// HandleHealthz responds 200 {"status":"healthy"}.
// Used by Docker / Kubernetes liveness probes.
func HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statusResponse{Status: "healthy"})
}

// HandleReadyz responds 200 {"status":"ready"} or 503 {"status":"not_ready"}.
// Reflects whether the background sync loops are successfully running.
func HandleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !IsReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(statusResponse{Status: "not_ready"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statusResponse{Status: "ready"})
}
