// Package handler is the HTTP surface for global-search-svc (DATA-06,
// ZS-SVC-N-001 §4). Every write goes through the store, which already
// writes its own outbox_events row in the same transaction as the
// business write — this layer never publishes directly. Authorization
// against a restricted document happens inside the store's SQL, never
// as a post-fetch filter here.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/global-search-svc/internal/authz"
	"zoiko.io/global-search-svc/internal/domain"
	svcenvelope "zoiko.io/global-search-svc/internal/envelope"
	"zoiko.io/global-search-svc/internal/health"
	customMiddleware "zoiko.io/global-search-svc/internal/middleware"
	"zoiko.io/global-search-svc/internal/store"
)

const (
	ActionSearchManage = "GLOBAL_SEARCH_MANAGE"
	ActionSearchRead   = "GLOBAL_SEARCH_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/global-search", func(r chi.Router) {
		r.Post("/indexes/{scope}:reindex", h.ReindexScope)
		r.Post("/indexes/{scope}:rebuild", h.RebuildIndex)
		r.Post("/indexes/{scope}:mark-available", h.MarkIndexAvailable)
		r.Get("/indexes/{scope}/health", h.GetIndexHealth)

		r.Post("/policies", h.SetSearchPolicy)

		r.Post("/documents", h.IndexObject)
		r.Delete("/documents", h.PurgeIndexObject)

		r.Get("/search", h.Search)
		r.Get("/autocomplete", h.Autocomplete)
		r.Get("/count", h.Count)
		r.Get("/explain", h.ExplainResult)
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

// ── Index lifecycle commands ─────────────────────────────────────────────────

func (h *Handler) ReindexScope(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	scope := chi.URLParam(r, "scope")
	claim := h.idempotencyClaim(principalID, "ReindexScope", scope, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.ReindexScope(r.Context(), tenantID, scope, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) RebuildIndex(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	scope := chi.URLParam(r, "scope")
	claim := h.idempotencyClaim(principalID, "RebuildIndex", scope, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.RebuildIndex(r.Context(), tenantID, scope, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) MarkIndexAvailable(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	scope := chi.URLParam(r, "scope")
	claim := h.idempotencyClaim(principalID, "MarkIndexAvailable", scope, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.MarkIndexAvailable(r.Context(), tenantID, scope, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) SetSearchPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	var req domain.SetSearchPolicyRequest
	if _, err := h.readBody(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.SetSearchPolicy(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Document commands ────────────────────────────────────────────────────────

func (h *Handler) IndexObject(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	var req domain.IndexObjectRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resourceKey := req.Scope + "|" + req.ObjectType + "|" + req.ObjectRef
	claim := h.idempotencyClaim(principalID, "IndexObject", resourceKey, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.IndexObject(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) PurgeIndexObject(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchManage) {
		return
	}
	var req domain.PurgeIndexObjectRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resourceKey := req.Scope + "|" + req.ObjectRef
	claim := h.idempotencyClaim(principalID, "PurgeIndexObject", resourceKey, reqHash, r.Header.Get("Idempotency-Key"))
	if err := h.store.PurgeIndexObject(r.Context(), tenantID, req, claim); err != nil {
		h.respondFromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchRead) {
		return
	}
	scope := r.URL.Query().Get("scope")
	queryText := r.URL.Query().Get("q")
	limit := parseLimit(r.URL.Query().Get("limit"), 20)
	got, err := h.store.Search(r.Context(), tenantID, scope, principalID, queryText, limit)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) Autocomplete(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchRead) {
		return
	}
	scope := r.URL.Query().Get("scope")
	prefix := r.URL.Query().Get("prefix")
	limit := parseLimit(r.URL.Query().Get("limit"), 10)
	got, err := h.store.Autocomplete(r.Context(), tenantID, scope, principalID, prefix, limit)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if got == nil {
		got = []string{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": got, "count": len(got)})
}

func (h *Handler) Count(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchRead) {
		return
	}
	scope := r.URL.Query().Get("scope")
	queryText := r.URL.Query().Get("q")
	got, err := h.store.Count(r.Context(), tenantID, scope, principalID, queryText)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, map[string]int{"count": got})
}

func (h *Handler) ExplainResult(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchRead) {
		return
	}
	scope := r.URL.Query().Get("scope")
	objectRef := r.URL.Query().Get("object_ref")
	got, err := h.store.ExplainResult(r.Context(), tenantID, scope, principalID, objectRef)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetIndexHealth(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSearchRead) {
		return
	}
	scope := chi.URLParam(r, "scope")
	got, err := h.store.GetIndexHealth(r.Context(), tenantID, scope, time.Now().UTC())
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func parseLimit(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
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
	case errors.Is(err, domain.ErrIndexNotFound), errors.Is(err, domain.ErrPolicyNotFound),
		errors.Is(err, domain.ErrDocumentNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("global-search-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
