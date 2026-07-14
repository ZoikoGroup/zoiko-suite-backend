// Package health provides the /healthz and /readyz HTTP handlers for
// search-indexer-svc. Follows the same pattern as obligations-svc and
// audit-event-store-svc (no external dependency required to report healthy).
package health

import (
	"encoding/json"
	"net/http"
)

type statusResponse struct {
	Status string `json:"status"`
}

// HandleHealthz responds 200 {"status":"healthy"}.
// Used by Docker / Kubernetes liveness probes.
func HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statusResponse{Status: "healthy"})
}

// HandleReadyz responds 200 {"status":"ready"}.
// Can be extended to gate on OpenSearch connectivity before marking ready.
func HandleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statusResponse{Status: "ready"})
}
