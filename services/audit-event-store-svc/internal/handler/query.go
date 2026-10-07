package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/store"
)

const AuditEventRead = "AUDIT_EVENT_READ"

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type QueryHandler struct {
	store store.Store
	authz AuthzChecker
	log   *zap.Logger
}

func NewQueryHandler(s store.Store, az AuthzChecker, log *zap.Logger) *QueryHandler {
	return &QueryHandler{
		store: s,
		authz: az,
		log:   log,
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (h *QueryHandler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "X-Tenant-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	if declared := r.URL.Query().Get("tenant_id"); declared != "" && declared != tenantID {
		writeError(w, http.StatusForbidden, "tenant_id in query does not match the verified X-Tenant-Id")
		return "", false
	}
	return tenantID, true
}

func (h *QueryHandler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

func (h *QueryHandler) authorize(w http.ResponseWriter, r *http.Request, principalID, scopeID string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, scopeID, AuditEventRead); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to read audit events")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

type eventResponse struct {
	EventID           string          `json:"event_id"`
	EventType         string          `json:"event_type"`
	TenantID          string          `json:"tenant_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	PrincipalID       string          `json:"principal_id,omitempty"`
	SourceService     string          `json:"source_service"`
	SchemaVersion     string          `json:"schema_version"`
	Payload           json.RawMessage `json:"payload"`
	StoredAt          time.Time       `json:"stored_at"`
	CorrelationID     string          `json:"correlation_id,omitempty"`
	CausationID       string          `json:"causation_id,omitempty"`
	SequenceNumber    int64           `json:"sequence_number"`
	PayloadHash       string          `json:"payload_hash,omitempty"`
	PreviousEventHash string          `json:"previous_event_hash,omitempty"`
}

type queryResultResponse struct {
	Events []eventResponse `json:"events"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func toEventResponse(e store.AuditEvent) eventResponse {
	return eventResponse{
		EventID:           e.EventID,
		EventType:         e.EventType,
		TenantID:          e.TenantID,
		LegalEntityID:     e.LegalEntityID,
		PrincipalID:       e.PrincipalID,
		SourceService:     e.SourceService,
		SchemaVersion:     e.SchemaVersion,
		Payload:           e.Payload,
		StoredAt:          e.StoredAt,
		CorrelationID:     e.CorrelationID,
		CausationID:       e.CausationID,
		SequenceNumber:    e.SequenceNumber,
		PayloadHash:       e.PayloadHash,
		PreviousEventHash: e.PreviousEventHash,
	}
}

// ListEvents handles GET /v1/events.
// Supports filtering by:
//   - legal_entity_id / entity
//   - principal_id / actor
//   - event_type / action
//   - correlation_id / workflow
//   - causation_id
//   - from / start_time
//   - to / end_time
//   - limit, offset
func (h *QueryHandler) ListEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()

	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" {
		legalEntityID = q.Get("entity")
	}

	actor := q.Get("principal_id")
	if actor == "" {
		actor = q.Get("actor")
	}

	action := q.Get("event_type")
	if action == "" {
		action = q.Get("action")
	}

	workflow := q.Get("correlation_id")
	if workflow == "" {
		workflow = q.Get("workflow")
	}

	causationID := q.Get("causation_id")

	limit := 50
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}

	offset := 0
	if o := q.Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	var startTime *time.Time
	fromStr := q.Get("from")
	if fromStr == "" {
		fromStr = q.Get("start_time")
	}
	if fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			startTime = &t
		} else {
			writeError(w, http.StatusBadRequest, "invalid from timestamp, must be RFC3339")
			return
		}
	}

	var endTime *time.Time
	toStr := q.Get("to")
	if toStr == "" {
		toStr = q.Get("end_time")
	}
	if toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			endTime = &t
		} else {
			writeError(w, http.StatusBadRequest, "invalid to timestamp, must be RFC3339")
			return
		}
	}

	// Authorize caller against legal entity if given, otherwise against tenant scope
	scope := legalEntityID
	if scope == "" {
		scope = tenantID
	}
	if !h.authorize(w, r, principalID, scope) {
		return
	}

	filter := store.AuditQueryFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		PrincipalID:   actor,
		EventType:     action,
		CorrelationID: workflow,
		CausationID:   causationID,
		StartTime:     startTime,
		EndTime:       endTime,
		Limit:         limit,
		Offset:        offset,
	}

	events, total, err := h.store.Query(r.Context(), filter)
	if err != nil {
		h.log.Error("ListEvents query failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal query error")
		return
	}

	resp := queryResultResponse{
		Events: make([]eventResponse, 0, len(events)),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}
	for _, e := range events {
		resp.Events = append(resp.Events, toEventResponse(e))
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetEvent handles GET /v1/events/{event_id}.
func (h *QueryHandler) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "event_id")
	if eventID == "" {
		writeError(w, http.StatusBadRequest, "event_id is required")
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

	e, err := h.store.GetByID(r.Context(), tenantID, eventID)
	if err != nil {
		h.log.Error("GetEvent store error", zap.String("event_id", eventID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal store error")
		return
	}
	if e == nil {
		writeError(w, http.StatusNotFound, "audit event not found")
		return
	}

	// Authorize access to this event's legal entity
	if !h.authorize(w, r, principalID, e.LegalEntityID) {
		return
	}

	writeJSON(w, http.StatusOK, toEventResponse(*e))
}
