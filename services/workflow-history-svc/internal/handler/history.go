// Package handler provides the HTTP read API for workflow transition history.
//
// Endpoints:
//
//	GET /v1/workflows/{workflow_instance_id}/history
//	  Returns the full chronological transition list for one workflow instance,
//	  scoped to the VERIFIED tenant from X-Tenant-Id (a tenant_id query
//	  parameter is still accepted but must match it). 404 if no events exist
//	  for the given instance ID within that tenant — indistinguishable from the instance
//	  belonging to a different tenant, so this endpoint cannot be used to
//	  probe for the existence of another tenant's workflow instances.
//
//	GET /v1/workflows/history?legal_entity_id=...&from=...&to=...
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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/workflow-history-svc/internal/authz"
	"zoiko.io/workflow-history-svc/internal/store"
)

// Handler exposes the workflow history read API.
type Handler struct {
	authz AuthzChecker
	read  store.ReadStore
	log   *zap.Logger
}

// New returns a Handler wired to the given ReadStore.
func New(read store.ReadStore, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{read: read, authz: az, log: log}
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

// Action constant passed to authorization-svc as action_type.
const WorkflowHistoryRead = "WORKFLOW_HISTORY_READ"

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// requireTenant returns the gateway-verified tenant, and refuses the request
// when there is none.
//
// This replaces reading tenant_id from the QUERY STRING, which is what both
// routes did — and it was not merely a missing check, it was a cross-tenant
// read. Nothing in this service ever read X-Tenant-Id at all.
//
// The chain mattered because this service DOES have row-level security:
//
//	handler:  tenantID := r.URL.Query().Get("tenant_id")   // caller-supplied
//	store:    set_config('app.tenant_id', tenantID, ...)   // via withRLS
//	policy:   USING (tenant_id = current_setting('app.tenant_id', true))
//
// So the policy was never bypassed — it was SATISFIED, with a value the
// caller chose. Postgres faithfully returned the rows of whatever tenant was
// named in the URL. That is the same security-theater shape as the five
// Priority 1b business services, but strictly worse: there a header-less
// caller landed in one shared synthetic bucket, whereas here a caller picks
// its victim by editing a query parameter.
//
// What that exposed is the workflow transition log — every state change,
// approval and event payload for another tenant's governed work.
//
// tenant_id is still accepted on the query string for URL compatibility, but
// it may now only AGREE with the verified header; disagreement is refused
// rather than silently resolved, so a caller that means to read another
// tenant gets an error instead of a quietly reinterpreted answer.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Tenant-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	if declared := r.URL.Query().Get("tenant_id"); declared != "" && declared != tenantID {
		writeError(w, http.StatusForbidden,
			"tenant_id in the query does not match the verified X-Tenant-Id")
		return "", false
	}
	return tenantID, true
}

// requirePrincipal returns the gateway-verified principal, or refuses.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

// authorize asks authorization-svc whether this principal may read workflow
// history within legalEntityID, and fails CLOSED.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, WorkflowHistoryRead); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to read workflow history")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
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
// 401 if X-Tenant-Id or X-Principal-Id is missing, 403 if a tenant_id query
// parameter disagrees with the verified header or authorization is denied,
// 404 if no events exist for the given instance ID within that tenant.
func (h *Handler) GetInstanceHistory(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "workflow_instance_id")
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, "workflow_instance_id is required")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	events, err := h.read.ListByInstance(r.Context(), tenantID, instanceID)
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

	// Authorized against the legal entity these events actually belong to,
	// read from the rows rather than from anything the caller supplied. The
	// store query above is already scoped to the verified tenant, so another
	// tenant's instance is indistinguishable from a nonexistent one (404)
	// before this point — which is also why authorizing here cannot leak the
	// existence of an instance the caller may not see.
	if !h.authorize(w, r, principalID, events[0].LegalEntityID) {
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
	legalEntityID := q.Get("legal_entity_id")
	fromStr := q.Get("from")
	toStr := q.Get("to")

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	// legal_entity_id was already mandatory on this route, which makes it a
	// usable authorization scope as-is — no API tightening needed here,
	// unlike the list routes in mtls/key-management/carta where it was
	// optional and therefore self-disabling.
	if !h.authorize(w, r, principalID, legalEntityID) {
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
