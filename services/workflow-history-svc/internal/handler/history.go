// Package handler provides the HTTP read API for workflow transition history.
//
// Endpoints:
//
//	GET /v1/workflows/{workflow_instance_id}/history
//	  Returns the full chronological transition list for one workflow instance.
//	  404 if no events exist for the given instance ID.
//
//	GET /v1/workflows/history?tenant_id=...&legal_entity_id=...&from=...&to=...
//	  Cross-workflow query for all transitions within a time window for a
//	  specific tenant and legal entity.
//
//	  v1 Known Gap: evidence-manifest-svc currently fetches workflow data
//	  directly from workflow-svc by workflow_instance_id and is NOT wired to
//	  this cross-workflow query endpoint. Wiring that cross-reference is a
//	  documented v1 scope constraint — see docs/architecture/known-gaps.md.
//	  The endpoint is fully functional; no upstream caller uses it in v1.
package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/store"
)

// Handler exposes the workflow history read API.
type Handler struct {
	read store.ReadStore
	log  *zap.Logger
}

// New returns a Handler wired to the given ReadStore.
func New(read store.ReadStore, log *zap.Logger) *Handler {
	return &Handler{read: read, log: log}
}

// historyEventResponse is the JSON shape returned by both read endpoints.
type historyEventResponse struct {
	EventID            string          `json:"event_id"`
	WorkflowInstanceID string          `json:"workflow_instance_id"`
	EventType          string          `json:"event_type"`
	CorrelationID      string          `json:"correlation_id"`
	TenantID           string          `json:"tenant_id"`
	LegalEntityID      string          `json:"legal_entity_id"`
	Payload            json.RawMessage `json:"payload"`
	RecordedAt         time.Time       `json:"recorded_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func toResponse(e store.WorkflowHistoryEvent) historyEventResponse {
	return historyEventResponse{
		EventID:            e.EventID,
		WorkflowInstanceID: e.WorkflowInstanceID,
		EventType:          e.EventType,
		CorrelationID:      e.CorrelationID,
		TenantID:           e.TenantID,
		LegalEntityID:      e.LegalEntityID,
		Payload:            e.Payload,
		RecordedAt:         e.RecordedAt,
	}
}

// GetInstanceHistory handles GET /v1/workflows/{workflow_instance_id}/history.
//
// Returns the full chronological transition list for one workflow instance,
// ordered by recorded_at ASC (earliest event first).
// 404 if no events exist for the given instance ID.
func (h *Handler) GetInstanceHistory(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "workflow_instance_id")
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, "workflow_instance_id is required")
		return
	}

	events, err := h.read.ListByInstance(r.Context(), instanceID)
	if err != nil {
		h.log.Error("GetInstanceHistory: store error",
			zap.String("workflow_instance_id", instanceID),
			zap.Error(err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "no history found for workflow_instance_id")
		return
	}

	resp := make([]historyEventResponse, len(events))
	for i, e := range events {
		resp[i] = toResponse(e)
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetCrossWorkflowHistory handles GET /v1/workflows/history.
//
// Query parameters (all required):
//   - tenant_id:       tenant scope for the query
//   - legal_entity_id: legal entity scope for the query
//   - from:            start of the time window (RFC3339)
//   - to:              end of the time window (RFC3339)
//
// Returns all workflow history events for the given tenant and entity within
// the specified time window, ordered by recorded_at ASC.
//
// v1 Known Gap: evidence-manifest-svc is not wired to this endpoint.
// See package doc for details.
func (h *Handler) GetCrossWorkflowHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenantID := q.Get("tenant_id")
	legalEntityID := q.Get("legal_entity_id")
	fromStr := q.Get("from")
	toStr := q.Get("to")

	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id is required")
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if fromStr == "" {
		writeError(w, http.StatusBadRequest, "from is required (RFC3339)")
		return
	}
	if toStr == "" {
		writeError(w, http.StatusBadRequest, "to is required (RFC3339)")
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from must be a valid RFC3339 timestamp")
		return
	}
	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "to must be a valid RFC3339 timestamp")
		return
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "to must be after from")
		return
	}

	filter := store.QueryFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		From:          from,
		To:            to,
	}

	events, err := h.read.ListByFilter(r.Context(), filter)
	if err != nil {
		h.log.Error("GetCrossWorkflowHistory: store error",
			zap.String("tenant_id", tenantID),
			zap.String("legal_entity_id", legalEntityID),
			zap.Error(err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := make([]historyEventResponse, len(events))
	for i, e := range events {
		resp[i] = toResponse(e)
	}

	// Return empty array (not null) when no events match, so callers can
	// distinguish "no history yet" from an error.
	if resp == nil {
		resp = []historyEventResponse{}
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Error: msg})
}
