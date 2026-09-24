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
// CommentModerated; MentionCreated; ThreadResolved." All five are
// implemented as of Wave 2. There is no "CommentDeleted" event — the
// doc's own event catalogue never names one; see
// domain.DeleteCommentParams's own doc comment.
type Publisher interface {
	PublishCommentAdded(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment, t domain.CommentThread)
	PublishCommentEdited(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment)
	PublishCommentModerated(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment)
	PublishMentionCreated(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, m domain.Mention)
	PublishThreadResolved(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, t domain.CommentThread)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// RetentionClient is DeleteComment's own real dependency on
// retention-registry-svc — see internal/retention's own doc comment.
// Left unconfigured, DeleteComment refuses with a clear error rather
// than a nil dereference or, worse, silently skipping the check.
type RetentionClient interface {
	IsBlocked(ctx context.Context, tenantID, commentID, actorID, correlationID string) (bool, error)
}

// ObjectVisibilityChecker answers "can this principal see this specific
// linked object" for exactly one linked_object_type. Mention needs this
// to satisfy the doc's own T29: "Unauthorized user mentioned in
// restricted thread receives object title -> Block mention/notification
// leakage." No generic object-instance ACL exists anywhere in this
// platform (confirmed against authorization-svc's own CheckAllowed,
// which is action-type + legal-entity scoped only) — so this is deliberately
// per-object-type and opt-in: each domain service that wants its objects
// to support safe mentions registers a checker for its own
// linked_object_type via WithVisibilityChecker. An object type with no
// registered checker refuses every mention against it — see Mention's own
// doc comment on why fail-closed is the only safe default here.
type ObjectVisibilityChecker interface {
	CanView(ctx context.Context, principalID, tenantID, legalEntityID, linkedObjectID string) (bool, error)
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
	store              store.Store
	publisher          Publisher
	authz              AuthZClient
	logger             *zap.Logger
	retention          RetentionClient
	visibilityCheckers map[string]ObjectVisibilityChecker
}

func New(st store.Store, pub Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger, visibilityCheckers: map[string]ObjectVisibilityChecker{}}
}

// WithRetentionClient sets DeleteComment's own real dependency on
// retention-registry-svc. Left unconfigured, DeleteComment refuses
// clearly — same never-optional-dependency posture as this platform's
// other WithXClient setters (e.g. inventory-management-svc's
// WithLedgerClient).
func (h *Handler) WithRetentionClient(c RetentionClient) *Handler {
	h.retention = c
	return h
}

// WithVisibilityChecker registers the CanView check Mention uses for one
// linked_object_type. See ObjectVisibilityChecker's own doc comment.
func (h *Handler) WithVisibilityChecker(linkedObjectType string, c ObjectVisibilityChecker) *Handler {
	h.visibilityCheckers[linkedObjectType] = c
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/threads", func(r chi.Router) {
		r.Post("/comments", h.AddComment)
		r.Get("/{thread_id}", h.GetThread)
		r.Get("/{thread_id}/comments", h.ListComments)
		r.Post("/{thread_id}/resolve", h.ResolveThread)
	})
	r.Route("/v1/comments", func(r chi.Router) {
		r.Post("/{comment_id}/edit", h.EditComment)
		r.Post("/{comment_id}/delete", h.DeleteComment)
		r.Get("/{comment_id}/history", h.GetCommentHistory)
		r.Post("/{comment_id}/mention", h.Mention)
		r.Get("/{comment_id}/mentions", h.GetMentions)
		r.Post("/{comment_id}/react", h.React)
		r.Post("/{comment_id}/moderate", h.Moderate)
		r.Get("/{comment_id}/moderation-trail", h.GetModerationTrail)
		r.Post("/{comment_id}/attachments", h.AttachReference)
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

type deleteCommentRequest struct {
	Reason string `json:"reason"`
}

// DeleteComment — BIZ-09's own DeleteComment command. Checks
// retention-registry-svc BEFORE the store call — the doc's own T28:
// "Deleted comment under legal hold disappears -> Block; retain
// restricted history." Fails closed on an unreachable
// retention-registry-svc, same posture as reporting-orchestration-svc's
// own client.
func (h *Handler) DeleteComment(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req deleteCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, domain.ErrReasonRequired.Error())
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
	if h.retention == nil {
		h.logger.Error("DeleteComment: retention client not configured")
		writeError(w, http.StatusServiceUnavailable, domain.ErrRetentionServiceUnavailable.Error())
		return
	}
	blocked, err := h.retention.IsBlocked(r.Context(), tenantID, commentID, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, domain.ErrRetentionServiceUnavailable.Error())
		return
	}
	if blocked {
		writeError(w, http.StatusConflict, domain.ErrRetentionHold.Error())
		return
	}

	comment, err := h.store.DeleteComment(r.Context(), domain.DeleteCommentParams{
		CommentID: commentID, TenantID: tenantID, ActorPrincipalID: principalID, Reason: req.Reason,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, comment)
}

type mentionRequest struct {
	MentionedPrincipalID string `json:"mentioned_principal_id"`
}

// Mention — BIZ-09's own Mention command. Refuses (fail-closed) unless a
// visibility checker is registered for the thread's linked_object_type
// AND that check grants the mentioned principal access — see
// ObjectVisibilityChecker's own doc comment. A refused mention writes no
// row at all, so a denied/unregistered mention leaves no trace an
// unauthorized caller could use to confirm the linked object exists.
func (h *Handler) Mention(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req mentionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MentionedPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "mentioned_principal_id is required")
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

	checker, registered := h.visibilityCheckers[thread.LinkedObjectType]
	if !registered {
		writeError(w, http.StatusForbidden, domain.ErrNoVisibilityCheckRegistered.Error())
		return
	}
	canView, err := checker.CanView(r.Context(), req.MentionedPrincipalID, tenantID, thread.LegalEntityID, thread.LinkedObjectID)
	if err != nil {
		h.logger.Error("visibility check failed", zap.String("linked_object_type", thread.LinkedObjectType), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "visibility check unavailable")
		return
	}
	if !canView {
		writeError(w, http.StatusForbidden, domain.ErrMentionNotVisible.Error())
		return
	}

	mention, err := h.store.Mention(r.Context(), domain.MentionParams{
		CommentID: commentID, TenantID: tenantID, MentionedPrincipalID: req.MentionedPrincipalID, VisibilityGranted: true,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	h.publisher.PublishMentionCreated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, tenantID, thread.LegalEntityID, *mention)
	writeJSON(w, http.StatusCreated, mention)
}

// GetMentions — BIZ-09's own GetMentions query.
func (h *Handler) GetMentions(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	list, err := h.store.GetMentions(r.Context(), tenantID, commentID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if list == nil {
		list = []domain.Mention{}
	}
	writeJSON(w, http.StatusOK, list)
}

type reactRequest struct {
	ReactionType string `json:"reaction_type"`
}

// React — BIZ-09's own React command. A toggle — see store.React's own
// doc comment.
func (h *Handler) React(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req reactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ReactionType == "" {
		writeError(w, http.StatusBadRequest, "reaction_type is required")
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
	added, err := h.store.React(r.Context(), domain.ReactParams{
		CommentID: commentID, TenantID: tenantID, PrincipalID: principalID, ReactionType: req.ReactionType,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"added": added})
}

type moderateRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// Moderate — BIZ-09's own Moderate command. Reason is mandatory — the
// doc's own T46.
func (h *Handler) Moderate(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req moderateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, domain.ErrReasonRequired.Error())
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
	comment, err := h.store.Moderate(r.Context(), domain.ModerateParams{
		CommentID: commentID, TenantID: tenantID, ModeratorPrincipalID: principalID, Action: req.Action, Reason: req.Reason,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	h.publisher.PublishCommentModerated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, tenantID, thread.LegalEntityID, *comment)
	writeJSON(w, http.StatusOK, comment)
}

// GetModerationTrail — BIZ-09's own GetModerationTrail query.
func (h *Handler) GetModerationTrail(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	list, err := h.store.GetModerationTrail(r.Context(), tenantID, commentID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if list == nil {
		list = []domain.ModerationEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

type resolveThreadRequest struct {
	ResolutionNote string `json:"resolution_note,omitempty"`
}

// ResolveThread — BIZ-09's own ResolveThread command.
func (h *Handler) ResolveThread(w http.ResponseWriter, r *http.Request) {
	threadID := chi.URLParam(r, "thread_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req resolveThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	current, err := h.store.GetThread(r.Context(), threadID)
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionCommentManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	thread, err := h.store.ResolveThread(r.Context(), domain.ResolveThreadParams{
		ThreadID: threadID, TenantID: tenantID, ActorPrincipalID: principalID, ResolutionNote: req.ResolutionNote,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	h.publisher.PublishThreadResolved(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, tenantID, thread.LegalEntityID, *thread)
	writeJSON(w, http.StatusOK, thread)
}

type attachReferenceRequest struct {
	LinkedObjectType string `json:"linked_object_type,omitempty"`
	LinkedObjectID   string `json:"linked_object_id"`
}

// AttachReference — BIZ-09's own AttachReference command. Never accepts
// a file upload — LinkedObjectID references a file already stored in
// document-vault-svc (or any other object-owning service).
func (h *Handler) AttachReference(w http.ResponseWriter, r *http.Request) {
	commentID := chi.URLParam(r, "comment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req attachReferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LinkedObjectID == "" {
		writeError(w, http.StatusBadRequest, "linked_object_id is required")
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
	attachment, err := h.store.AttachReference(r.Context(), domain.AttachReferenceParams{
		CommentID: commentID, TenantID: tenantID, LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID,
		AttachedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeCommentErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, attachment)
}

func (h *Handler) writeCommentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrThreadNotFound):
		writeError(w, http.StatusNotFound, "thread not found")
	case errors.Is(err, domain.ErrCommentNotFound):
		writeError(w, http.StatusNotFound, "comment not found")
	case errors.Is(err, domain.ErrCommentInvalidState):
		writeError(w, http.StatusConflict, "comment is not in a state that permits this action")
	case errors.Is(err, domain.ErrThreadInvalidState):
		writeError(w, http.StatusConflict, "thread is not in a state that permits this action")
	case errors.Is(err, domain.ErrEmptyBody):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrParentCommentNotInThread):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrReasonRequired):
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
