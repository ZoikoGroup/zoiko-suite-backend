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

// actionDeadlineManage covers every write command except CreateDeadline/
// MirrorAuthoritativeDeadline (their own actionDeadlineCreate) — same
// breadth as BIZ-05's own actionTaskManage, which likewise covers
// assign/start/block/escalate/complete/reopen/cancel/list uniformly.
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
		r.Get("/upcoming", dh.ListUpcoming)
		r.Get("/overdue", dh.ListOverdue)
		r.Get("/{deadline_id}", dh.GetDeadline)
		r.Get("/{deadline_id}/source", dh.GetSourceDeadline)
		r.Get("/{deadline_id}/explain", dh.ExplainCalculation)
		r.Post("/{deadline_id}/assign-owner", dh.AssignOwner)
		r.Post("/{deadline_id}/complete", dh.CompleteDeadline)
		r.Post("/{deadline_id}/recalculate", dh.Recalculate)
		r.Post("/{deadline_id}/waive", dh.Waive)
		r.Post("/{deadline_id}/cancel", dh.CancelDeadline)
		r.Post("/{deadline_id}/escalate", dh.Escalate)
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

type recalculateRequest struct {
	DueAt      time.Time `json:"due_at"`
	CalcRule   string    `json:"calc_rule,omitempty"`
	CalcInputs string    `json:"calc_inputs,omitempty"`
}

// Recalculate — BIZ-08's own Recalculate command. Refused outright on a
// mirrored deadline by the store; see
// domain.ErrCannotRecalculateMirroredDeadline's own doc comment.
func (h *deadlineHandler) Recalculate(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req recalculateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DueAt.IsZero() {
		writeError(w, http.StatusBadRequest, "due_at is required")
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
	d, err := h.store.Recalculate(r.Context(), domain.RecalculateParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID,
		DueAt: req.DueAt, CalcRule: req.CalcRule, CalcInputs: req.CalcInputs,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type waiveRequest struct {
	Reason string `json:"reason"`
}

// Waive — BIZ-08's own Waive command.
func (h *deadlineHandler) Waive(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req waiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
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
	d, err := h.store.Waive(r.Context(), domain.WaiveParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, Reason: req.Reason,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type cancelDeadlineRequest struct {
	Reason string `json:"reason"`
}

// CancelDeadline — BIZ-08's own Cancel command.
func (h *deadlineHandler) CancelDeadline(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req cancelDeadlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
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
	d, err := h.store.CancelDeadline(r.Context(), domain.CancelDeadlineParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, Reason: req.Reason,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type escalateRequest struct {
	EscalatedToRole string `json:"escalated_to_role"`
	Reason          string `json:"reason"`
}

// Escalate — BIZ-08's own Escalate command. Does not change the
// deadline's own status — see domain.EscalateParams's own doc comment.
func (h *deadlineHandler) Escalate(w http.ResponseWriter, r *http.Request) {
	deadlineID := chi.URLParam(r, "deadline_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req escalateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.EscalatedToRole == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "escalated_to_role and reason are required")
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
	esc, err := h.store.Escalate(r.Context(), domain.EscalateParams{
		DeadlineID: deadlineID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID,
		EscalatedToRole: req.EscalatedToRole, Reason: req.Reason,
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, esc)
}

// ListUpcoming — BIZ-08's own ListUpcoming query. Publishes
// DeadlineDueSoon for exactly the deadlines this call is the first to
// observe crossing the threshold — see store.ListUpcoming's own doc
// comment.
func (h *deadlineHandler) ListUpcoming(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionDeadlineManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	deadlines, justNotifiedIDs, err := h.store.ListUpcoming(r.Context(), domain.ListUpcomingParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: legalEntityID, OwnerPrincipalID: r.URL.Query().Get("owner_principal_id"),
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	for _, id := range justNotifiedIDs {
		if err := h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "deadline.due_soon", CaseID: id, TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: legalEntityID,
			ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: map[string]string{"deadline_id": id},
		}); err != nil {
			h.logger.Warn("failed to publish deadline.due_soon event", zap.Error(err))
		}
	}
	if deadlines == nil {
		deadlines = []domain.Deadline{}
	}
	writeJSON(w, http.StatusOK, deadlines)
}

// ListOverdue — BIZ-08's own ListOverdue query. Publishes DeadlineOverdue
// for exactly the deadlines this call is the first to observe as
// overdue — see store.ListOverdue's own doc comment.
func (h *deadlineHandler) ListOverdue(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionDeadlineManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	deadlines, justNotifiedIDs, err := h.store.ListOverdue(r.Context(), domain.ListOverdueParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: legalEntityID, OwnerPrincipalID: r.URL.Query().Get("owner_principal_id"),
	})
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	for _, id := range justNotifiedIDs {
		if err := h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "deadline.overdue", CaseID: id, TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: legalEntityID,
			ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: map[string]string{"deadline_id": id},
		}); err != nil {
			h.logger.Warn("failed to publish deadline.overdue event", zap.Error(err))
		}
	}
	if deadlines == nil {
		deadlines = []domain.Deadline{}
	}
	writeJSON(w, http.StatusOK, deadlines)
}

// ExplainCalculation — BIZ-08's own ExplainCalculation query.
func (h *deadlineHandler) ExplainCalculation(w http.ResponseWriter, r *http.Request) {
	res, err := h.store.ExplainCalculation(r.Context(), middleware.GetTenantID(r.Context()), chi.URLParam(r, "deadline_id"))
	if err != nil {
		h.writeDeadlineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
