package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/board-resolutions-svc/internal/authz"
	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/events"
	"zoiko.io/board-resolutions-svc/internal/middleware"
	"zoiko.io/board-resolutions-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/meetings", func(r chi.Router) {
		r.Post("/", h.CreateMeeting)
		r.Get("/", h.ListMeetings)
		r.Get("/{id}", h.GetMeeting)
	})

	r.Route("/v1/resolutions", func(r chi.Router) {
		r.Post("/", h.CreateResolution)
		r.Get("/", h.ListResolutions)
		r.Get("/{id}", h.GetResolution)
		r.Post("/{id}/vote", h.RecordVotes)
		r.Post("/{id}/pass", h.PassResolution)
	})
}

// --- Meeting Handlers ---

func (h *Handler) CreateMeeting(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateMeetingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ScheduledAt.IsZero() {
		writeError(w, http.StatusBadRequest, "title and scheduled_at are required")
		return
	}

	m := &domain.BoardMeeting{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Title:         req.Title,
		ScheduledAt:   req.ScheduledAt,
		Location:      req.Location,
		EffectiveFrom: req.EffectiveFrom,
		CreatedBy:     req.CreatedBy,
	}

	if err := h.store.CreateMeeting(r.Context(), m); err != nil {
		h.logger.Error("create meeting failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create meeting")
		return
	}

	_ = h.publisher.Publish(r.Context(), "meeting.created", m.MeetingID, tenantID, m)
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) GetMeeting(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := h.store.GetMeeting(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrMeetingNotFound) {
			writeError(w, http.StatusNotFound, "meeting not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get meeting")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ListMeetings(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	meetings, err := h.store.ListMeetings(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list meetings")
		return
	}
	if meetings == nil {
		meetings = []domain.BoardMeeting{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"meetings": meetings, "total": len(meetings)})
}

// --- Resolution Handlers ---

func (h *Handler) CreateResolution(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Content == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "title, content, and category are required")
		return
	}

	res := &domain.BoardResolution{
		MeetingID:        req.MeetingID,
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		ResolutionNumber: req.ResolutionNumber,
		Title:            req.Title,
		Content:          req.Content,
		Category:         req.Category,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		CreatedBy:        req.CreatedBy,
	}

	if err := h.store.CreateResolution(r.Context(), res); err != nil {
		h.logger.Error("create resolution failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create resolution")
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.created", res.ResolutionID, tenantID, res)
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) GetResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrResolutionNotFound) {
			writeError(w, http.StatusNotFound, "resolution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get resolution")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) ListResolutions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	meetingID := r.URL.Query().Get("meeting_id")
	status := r.URL.Query().Get("status")
	resolutions, err := h.store.ListResolutions(r.Context(), legalEntityID, meetingID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list resolutions")
		return
	}
	if resolutions == nil {
		resolutions = []domain.BoardResolution{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"resolutions": resolutions, "total": len(resolutions)})
}

func (h *Handler) RecordVotes(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.RecordVotesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	res, err := h.store.RecordVotes(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrResolutionNotFound):
			writeError(w, http.StatusNotFound, "resolution not found")
		case errors.Is(err, domain.ErrResolutionAlreadyFinalized):
			writeError(w, http.StatusConflict, "resolution is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to record votes")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.votes_recorded", id, tenantID, res)
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) PassResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.PassResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.PassedBy == "" {
		writeError(w, http.StatusBadRequest, "passed_by is required")
		return
	}

	res, err := h.store.PassResolution(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrResolutionNotFound):
			writeError(w, http.StatusNotFound, "resolution not found")
		case errors.Is(err, domain.ErrResolutionAlreadyFinalized):
			writeError(w, http.StatusConflict, "resolution is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to pass resolution")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.passed", id, tenantID, res)
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
