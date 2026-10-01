// Package handler provides HTTP endpoints for audit-event-store-svc,
// including AUD-10 archive/verify capabilities and Doc 03 §14.1 query API.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/envelope"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// Store combines archive management and event querying.
type Store interface {
	store.ArchiveStore
	store.EventQueryStore
}

type Handler struct {
	store Store
	log   *zap.Logger
}

func New(s Store, log *zap.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// RegisterRoutes mounts the service's HTTP routes on r.
func RegisterRoutes(r chi.Router, h *Handler) {
	// Doc 03 §14.1 Event Query API and Live Hash Chain Verification
	r.Get("/v1/events", h.listEvents)
	r.Post("/v1/events/verify", h.verifyEventsChain)

	// AUD-10 archive/verify routes
	r.Post("/v1/archives", h.createArchive)
	r.Get("/v1/archives/{id}", h.getArchive)
	r.Post("/v1/archives/{id}/verify", h.verifyArchive)
	r.Get("/v1/archives/{id}/verifications", h.listVerifications)
}

// requireActor reads the acting principal already validated by the envelope middleware.
func requireActor(r *http.Request) string {
	e, _ := envelope.FromContext(r.Context())
	return e.Actor()
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, _ := envelope.FromContext(ctx)

	tenantID := e.TenantID
	if tenantID == "" {
		tenantID = r.Header.Get("X-Tenant-Id")
	}
	if tenantID == "" {
		tenantID = r.URL.Query().Get("tenant_id")
	}
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_required", "X-Tenant-Id header is required")
		return
	}

	q := r.URL.Query()
	params := domain.QueryEventsParams{
		TenantID:      tenantID,
		LegalEntityID: q.Get("entity"),
		PrincipalID:   q.Get("actor"),
		EventType:     q.Get("action"),
		CorrelationID: q.Get("workflow"),
		Limit:         50,
		Offset:        0,
	}

	if params.LegalEntityID == "" {
		params.LegalEntityID = q.Get("legal_entity_id")
	}
	if params.PrincipalID == "" {
		params.PrincipalID = q.Get("principal_id")
	}
	if params.EventType == "" {
		params.EventType = q.Get("event_type")
	}
	if params.CorrelationID == "" {
		params.CorrelationID = q.Get("correlation_id")
	}

	if limitStr := q.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			params.Limit = l
		}
	}
	if offsetStr := q.Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			params.Offset = o
		}
	}

	if fromStr := q.Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			params.FromTime = &t
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			params.ToTime = &t
		}
	}

	result, err := h.store.QueryEvents(ctx, params)
	if err != nil {
		h.log.Error("failed to query audit events", zap.Error(err), zap.String("tenant_id", tenantID))
		writeError(w, http.StatusInternalServerError, "query_failed", "failed to query audit events")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

type verifyChainResponse struct {
	Verified      bool   `json:"verified"`
	CheckedEvents int64  `json:"checkedEvents"`
	CheckedCount  int64  `json:"checked_events"`
	Timestamp     string `json:"timestamp"`
}

func (h *Handler) verifyEventsChain(w http.ResponseWriter, r *http.Request) {
	verified, count, err := h.store.VerifyChain(r.Context())
	if err != nil {
		h.log.Error("failed to verify events chain", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "verify_failed", "failed to verify chain")
		return
	}
	writeJSON(w, http.StatusOK, verifyChainResponse{
		Verified:      verified,
		CheckedEvents: count,
		CheckedCount:  count,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
	})
}

type createArchiveRequest struct {
	FromSequence int64 `json:"from_sequence"`
	ToSequence   int64 `json:"to_sequence"`
}

func (h *Handler) createArchive(w http.ResponseWriter, r *http.Request) {
	var req createArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	a, err := h.store.CreateArchive(r.Context(), domain.CreateArchiveParams{
		FromSequence:         req.FromSequence,
		ToSequence:           req.ToSequence,
		CreatedByPrincipalID: requireActor(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) getArchive(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.GetArchive(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) verifyArchive(w http.ResponseWriter, r *http.Request) {
	v, err := h.store.VerifyArchive(r.Context(), domain.VerifyArchiveParams{
		ArchiveID:             chi.URLParam(r, "id"),
		VerifiedByPrincipalID: requireActor(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) listVerifications(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListVerifications(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrArchiveNotFound):
		writeError(w, http.StatusNotFound, "archive_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRange):
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
	case errors.Is(err, domain.ErrEmptyRange):
		writeError(w, http.StatusBadRequest, "empty_range", err.Error())
	case errors.Is(err, domain.ErrChainBroken):
		writeError(w, http.StatusConflict, "chain_broken", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
