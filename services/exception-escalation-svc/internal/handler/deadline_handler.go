package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

const (
	actionDeadlineCreate = "DEADLINE_CREATE"
	actionDeadlineManage = "DEADLINE_MANAGE"
)

// deadlineHandler embeds *Handler so it reuses requirePrincipal/
// writeAuthzErr without duplicating them — same composition pattern used
// for BIZ-05's own taskHandler.
type deadlineHandler struct {
	*Handler
	store store.DeadlineStore
}

func RegisterDeadlineRoutes(r chi.Router, h *Handler, deadlineStore store.DeadlineStore) {
	dh := &deadlineHandler{Handler: h, store: deadlineStore}
	r.Route("/v1/deadlines", func(r chi.Router) {
		r.Post("/", dh.CreateDeadline)
		r.Post("/mirror", dh.MirrorAuthoritativeDeadline)
		r.Get("/{deadline_id}", dh.GetDeadline)
		r.Get("/{deadline_id}/source", dh.GetSourceDeadline)
		r.Post("/{deadline_id}/assign-owner", dh.AssignOwner)
		r.Post("/{deadline_id}/complete", dh.CompleteDeadline)
	})
}

type createDeadlineRequest struct {
	LegalEntityID    string    `json:"legal_entity_id"`
	Title            string    `json:"title"`
	LinkedObjectType string    `json:"linked_object_type,omitempty"`
	LinkedObjectID   string    `json:"linked_object_id,omitempty"`
	DueAt            time.Time `json:"due_at"`
	OwnerPrincipalID string    `json:"owner_principal_id,omitempty"`
	CalcRule         string    `json:"calc_rule,omitempty"`
	CalcInputs       string    `json:"calc_inputs,omitempty"`
}

// CreateDeadline — BIZ-08's own CreateDeadline command.
func (h *deadlineHandler) CreateDeadline(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createDeadlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Title == "" || req.DueAt.IsZero() {
		writeError(w, http.StatusBadRequest, "legal_entity_id, title, and due_at are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDeadlineCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	d, err := h.store.CreateDeadline(r.Context(), domain.CreateDeadlineParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID, Title: req.Title,
		LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID, DueAt: req.DueAt,
		OwnerPrincipalID: req.OwnerPrincipalID, CalcRule: req.CalcRule, CalcInputs: req.CalcInputs,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "deadline.created", CaseID: d.DeadlineID, TenantID: d.TenantID, LegalEntityID: d.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	}); err != nil {
		h.logger.Warn("failed to publish deadline.created event", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, d)
}

type mirrorAuthoritativeDeadlineRequest struct {
	LegalEntityID    string    `json:"legal_entity_id"`
	Title            string    `json:"title"`
	LinkedObjectType string    `json:"linked_object_type,omitempty"`
	LinkedObjectID   string    `json:"linked_object_id,omitempty"`
	DueAt            time.Time `json:"due_at"`
	OwnerPrincipalID string    `json:"owner_principal_id,omitempty"`
	SourceType       string    `json:"source_type"`
	SourceRef        string    `json:"source_ref"`
	SourceVersion    string    `json:"source_version"`
}

// MirrorAuthoritativeDeadline — BIZ-08's own MirrorAuthoritativeDeadline
// command. See domain.MirrorAuthoritativeDeadlineParams's own doc
// comment for the supersede-vs-conflict decision the store makes.
func (h *deadlineHandler) MirrorAuthoritativeDeadline(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req mirrorAuthoritativeDeadlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Title == "" || req.DueAt.IsZero() {
		writeError(w, http.StatusBadRequest, "legal_entity_id, title, and due_at are required")
		return
	}
	if req.SourceType == "" || req.SourceRef == "" || req.SourceVersion == "" {
		writeError(w, http.StatusBadRequest, domain.ErrDeadlineSourceRequired.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDeadlineCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	d, supersededDeadlineID, err := h.store.MirrorAuthoritativeDeadline(r.Context(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID, Title: req.Title,
		LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID, DueAt: req.DueAt,
		OwnerPrincipalID: req.OwnerPrincipalID, SourceType: req.SourceType, SourceRef: req.SourceRef, SourceVersion: req.SourceVersion,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "deadline.created", CaseID: d.DeadlineID, TenantID: d.TenantID, LegalEntityID: d.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	}); err != nil {
		h.logger.Warn("failed to publish deadline.created event", zap.Error(err))
	}
	if supersededDeadlineID != "" {
		if err := h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "deadline.superseded", CaseID: supersededDeadlineID, TenantID: d.TenantID, LegalEntityID: d.LegalEntityID,
			ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"),
			Payload: map[string]string{"deadline_id": supersededDeadlineID, "superseded_by_deadline_id": d.DeadlineID},
		}); err != nil {
			h.logger.Warn("failed to publish deadline.superseded event", zap.Error(err))
		}
	}
	writeJSON(w, http.StatusCreated, d)
}

// GetDeadline — BIZ-08's own GetDeadline query.
func (h *deadlineHandler) GetDeadline(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetDeadline(r.Context(), chi.URLParam(r, "deadline_id"))
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// GetSourceDeadline — BIZ-08's own GetSourceDeadline query.
func (h *deadlineHandler) GetSourceDeadline(w http.ResponseWriter, r *http.Request) {
	info, err := h.store.GetSourceDeadline(r.Context(), middleware.GetTenantID(r.Context()), chi.URLParam(r, "deadline_id"))
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type assignOwnerRequest struct {
	OwnerPrincipalID string `json:"owner_principal_id"`
}

// AssignOwner — BIZ-08's own AssignOwner command.
func (h *deadlineHandler) AssignOwner(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req assignOwnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OwnerPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "owner_principal_id is required")
		return
	}
	current, err := h.store.GetDeadline(r.Context(), deadlineID)
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionDeadlineManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	d, err := h.store.AssignOwner(r.Context(), domain.AssignOwnerParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, OwnerPrincipalID: req.OwnerPrincipalID,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// CompleteDeadline — BIZ-08's own Complete command.
func (h *deadlineHandler) CompleteDeadline(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	current, err := h.store.GetDeadline(r.Context(), deadlineID)
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionDeadlineManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	d, err := h.store.CompleteDeadline(r.Context(), domain.CompleteDeadlineParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "deadline.completed", CaseID: d.DeadlineID, TenantID: d.TenantID, LegalEntityID: d.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	}); err != nil {
		h.logger.Warn("failed to publish deadline.completed event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *deadlineHandler) writeDeadlineErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDeadlineNotFound):
		writeError(w, http.StatusNotFound, "deadline not found")
	case errors.Is(err, domain.ErrDeadlineInvalidState):
		writeError(w, http.StatusConflict, "deadline is not in a state that permits this action")
	case errors.Is(err, domain.ErrDeadlineSourceRequired):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrDeadlineSourceConflict):
		writeError(w, http.StatusConflict, "DEADLINE_SOURCE_CONFLICT: "+err.Error())
	case errors.Is(err, domain.ErrCannotRecalculateMirroredDeadline):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("deadline store error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "deadline store unavailable")
	}
}
