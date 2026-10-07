// Package handler is the HTTP surface for assistant-rag-svc (AI-03,
// ZS-SVC-N-001 §4/§13 Wave 7). Every write goes through the store,
// which already writes its own outbox_events row in the same
// transaction as the business write — this layer never publishes
// directly.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/assistant-rag-svc/internal/authz"
	"zoiko.io/assistant-rag-svc/internal/domain"
	svcenvelope "zoiko.io/assistant-rag-svc/internal/envelope"
	"zoiko.io/assistant-rag-svc/internal/health"
	customMiddleware "zoiko.io/assistant-rag-svc/internal/middleware"
	"zoiko.io/assistant-rag-svc/internal/store"
)

const (
	ActionAssistantManage = "ASSISTANT_MANAGE"
	ActionAssistantRead   = "ASSISTANT_READ"
)

type Handler struct {
	store  store.Store
	authz  *authz.Client
	logger *zap.Logger
}

func NewHandler(s store.Store, a *authz.Client, l *zap.Logger) *Handler {
	return &Handler{store: s, authz: a, logger: l}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.Handler())

	r.With(customMiddleware.TenantMiddleware).Route("/v1/assistant", func(r chi.Router) {
		r.Post("/source-grants", h.RegisterSourceGrant)
		r.Post("/tool-policies", h.RegisterToolPolicy)

		r.Post("/sessions", h.StartSession)
		r.Get("/sessions/{sessionID}", h.GetSession)
		r.Post("/sessions/{sessionID}:end", h.EndSession)

		r.Post("/retrieve", h.Retrieve)
		r.Get("/retrieval-sets/{retrievalSetID}/items", h.GetRetrievedItems)

		r.Post("/ask", h.Ask)
		r.Get("/executions/{executionID}/citations", h.GetCitations)
		r.Get("/executions/{executionID}/evidence", h.GetEvidence)

		r.Post("/tool-proposals", h.ProposeToolCall)
		r.Get("/tool-proposals/{proposalID}", h.GetToolProposal)
		r.Post("/tool-proposals/{proposalID}:approve", h.ApproveToolProposal)
		r.Post("/tool-proposals/{proposalID}:reject", h.RejectToolProposal)
		r.Post("/tool-proposals/{proposalID}:execute", h.ExecuteApprovedToolCall)
	})

	return r
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.respondError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		h.respondError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	h.respondError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func (h *Handler) readBody(r *http.Request, v interface{}) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Handler) idempotencyClaim(principalID, operation, resourceID, requestSHA256, key string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: principalID, Key: key,
		Operation: operation, RequestSHA256: requestSHA256, ResourceID: resourceID,
	}
}

// ── Governance registries ───────────────────────────────────────────────────

func (h *Handler) RegisterSourceGrant(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.RegisterSourceGrantRequest
	if _, err := h.readBody(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.RegisterSourceGrant(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RegisterToolPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.RegisterToolPolicyRequest
	if _, err := h.readBody(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.RegisterToolPolicy(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── AssistantSession ─────────────────────────────────────────────────────────

func (h *Handler) StartSession(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.StartSessionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "StartSession", req.SubjectID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.StartSession(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantRead) {
		return
	}
	got, err := h.store.GetSession(r.Context(), tenantID, chi.URLParam(r, "sessionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) EndSession(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	var req struct {
		Status domain.SessionStatus `json:"status"`
	}
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "EndSession", sessionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.EndSession(r.Context(), tenantID, sessionID, req.Status, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Retrieve ─────────────────────────────────────────────────────────────────

func (h *Handler) Retrieve(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.RetrieveRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "Retrieve", req.SessionID+"|"+req.Query, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.Retrieve(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetRetrievedItems(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantRead) {
		return
	}
	items, err := h.store.GetRetrievedItems(r.Context(), tenantID, chi.URLParam(r, "retrievalSetID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.RetrievedItem{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

// ── Ask ──────────────────────────────────────────────────────────────────────

func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.AskRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "Ask", req.SessionID+"|"+req.RetrievalSetID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.Ask(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetCitations(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantRead) {
		return
	}
	items, err := h.store.GetCitations(r.Context(), tenantID, chi.URLParam(r, "executionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.Citation{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetEvidence(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantRead) {
		return
	}
	got, err := h.store.GetEvidence(r.Context(), tenantID, chi.URLParam(r, "executionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── ToolProposal ─────────────────────────────────────────────────────────────

func (h *Handler) ProposeToolCall(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	var req domain.ProposeToolCallRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "ProposeToolCall", req.SessionID+"|"+req.ToolName, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ProposeToolCall(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetToolProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantRead) {
		return
	}
	got, err := h.store.GetToolProposal(r.Context(), tenantID, chi.URLParam(r, "proposalID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) ApproveToolProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	proposalID := chi.URLParam(r, "proposalID")
	var req domain.DecideToolProposalRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "ApproveToolProposal", proposalID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ApproveToolProposal(r.Context(), tenantID, proposalID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RejectToolProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	proposalID := chi.URLParam(r, "proposalID")
	var req domain.DecideToolProposalRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RejectToolProposal", proposalID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RejectToolProposal(r.Context(), tenantID, proposalID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) ExecuteApprovedToolCall(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionAssistantManage) {
		return
	}
	proposalID := chi.URLParam(r, "proposalID")
	claim := h.idempotencyClaim(principalID, "ExecuteApprovedToolCall", proposalID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.ExecuteApprovedToolCall(r.Context(), tenantID, proposalID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Response helpers ─────────────────────────────────────────────────────────

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) respondError(w http.ResponseWriter, code int, message string) {
	h.respondJSON(w, code, map[string]string{"error": message})
}

func (h *Handler) respondFromError(w http.ResponseWriter, err error) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		h.respondJSON(w, http.StatusOK, map[string]string{"resource_id": replay.ResourceID})
		return
	}
	switch {
	case errors.Is(err, domain.ErrSessionNotFound), errors.Is(err, domain.ErrRetrievalSetNotFound),
		errors.Is(err, domain.ErrToolPolicyNotFound), errors.Is(err, domain.ErrToolProposalNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrSessionNotActive), errors.Is(err, domain.ErrCitationSourceNotAuthorized),
		errors.Is(err, domain.ErrToolProposalNotPending), errors.Is(err, domain.ErrToolProposalNotApproved),
		errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("assistant-rag-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
