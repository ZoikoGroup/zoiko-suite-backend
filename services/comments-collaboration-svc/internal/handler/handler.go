// Package handler exposes comments-collaboration-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	svcmiddleware "zoiko.io/comments-collaboration-svc/internal/middleware"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

// Publisher is the event-publishing contract the handler depends on —
// the spec's own named Events: "CommentAdded; CommentEdited;
// CommentModerated; MentionCreated; ThreadResolved." Wave 1 implements
// the first two.
type Publisher interface {
	PublishCommentAdded(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment, t domain.CommentThread)
	PublishCommentEdited(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc — the platform's own
// CATALOG_*/DEADLINE_*-style namespace convention, applied here as
// COMMENT_*. actionCommentReadRestricted is deliberately distinct from
// actionCommentRead — the doc's own authorization note: "restricted
// threads require purpose and membership controls." See migration
// 000001's own doc comment on why this is the honest, minimal mechanism
// for that requirement rather than a fabricated membership subsystem.
const (
	actionCommentCreate         = "COMMENT_CREATE"
	actionCommentManage         = "COMMENT_MANAGE"
	actionCommentRead           = "COMMENT_READ"
	actionCommentReadRestricted = "COMMENT_READ_RESTRICTED"
)

type Handler struct {
	store     store.Store
	publisher Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/threads", func(r chi.Router) {
		r.Post("/comments", h.AddComment)
		r.Get("/{thread_id}", h.GetThread)
		r.Get("/{thread_id}/comments", h.ListComments)
	})
	r.Route("/v1/comments", func(r chi.Router) {
		r.Post("/{comment_id}/edit", h.EditComment)
		r.Get("/{comment_id}/history", h.GetCommentHistory)
	})
}

type addCommentRequest struct {
	LegalEntityID    string `json:"legal_entity_id"`
	LinkedObjectType string `json:"linked_object_type"`
	LinkedObjectID   string `json:"linked_object_id"`
	Restricted       bool   `json:"restricted,omitempty"`
	ParentCommentID  string `json:"parent_comment_id,omitempty"`
	Body             string `json:"body"`
}

// AddComment — BIZ-09's own AddComment command.
func (h *Handler) AddComment(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req addCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.LinkedObjectType == "" || req.LinkedObjectID == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, linked_object_type, linked_object_id and body are required")
		return
	}
	action := actionCommentCreate
	if req.Restricted {
		action = actionCommentReadRestricted
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	comment, thread, err := h.store.AddComment(r.Context(), domain.AddCommentParams{
		TenantID: svcmiddleware.TenantFromContext(r.Context()), LegalEntityID: req.LegalEntityID,
		LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID, Restricted: req.Restricted,
		ParentCommentID: req.ParentCommentID, Body: req.Body, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	h.publisher.PublishCommentAdded(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, thread.TenantID, thread.LegalEntityID, *comment, *thread)
	writeJSON(w, http.StatusCreated, map[string]any{"comment": comment, "thread": thread})
}

type editCommentRequest struct {
	Body string `json:"body"`
}

// EditComment — BIZ-09's own EditComment command. Content is fetched
// (read-only) before authorization and before the mutating store call —
// fetch-then-authorize-then-mutate throughout.
func (h *Handler) EditComment(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req editCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	_, thread, err := h.store.GetComment(r.Context(), tenantID, commentID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, thread.LegalEntityID, actionCommentManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	comment, err := h.store.EditComment(r.Context(), domain.EditCommentParams{
		CommentID: commentID, TenantID: tenantID, ActorPrincipalID: principalID, Body: req.Body,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	h.publisher.PublishCommentEdited(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, tenantID, thread.LegalEntityID, *comment)
	writeJSON(w, http.StatusOK, comment)
}

// GetThread — BIZ-09's own GetThread query.
func (h *Handler) GetThread(w http.ResponseWriter, r *http.Request) {
	t, err := h.store.GetThread(r.Context(), chi.URLParam(r, "thread_id"))
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ListComments — BIZ-09's own ListComments query.
func (h *Handler) ListComments(w http.ResponseWriter, r *http.Request) {
	threadID := chi.URLParam(r, "thread_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	list, err := h.store.ListComments(r.Context(), tenantID, threadID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if list == nil {
		list = []domain.Comment{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetCommentHistory — BIZ-09's own GetCommentHistory query.
func (h *Handler) GetCommentHistory(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	history, err := h.store.GetCommentHistory(r.Context(), tenantID, commentID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if history == nil {
		history = []domain.CommentVersion{}
	}
	writeJSON(w, http.StatusOK, history)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	writeError(w, http.StatusForbidden, "authorization denied: "+err.Error())
}

func (h *Handler) writeCommentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrThreadNotFound):
		writeError(w, http.StatusNotFound, "thread not found")
	case errors.Is(err, domain.ErrCommentNotFound):
		writeError(w, http.StatusNotFound, "comment not found")
	case errors.Is(err, domain.ErrCommentInvalidState):
		writeError(w, http.StatusConflict, "comment is not in a state that permits this action")
	case errors.Is(err, domain.ErrEmptyBody):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrParentCommentNotInThread):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		h.logger.Error("comments store error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "comments store unavailable")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
