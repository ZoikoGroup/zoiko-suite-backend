// Package handler is the HTTP surface for reconciliation-engine-svc
// (DATA-07, ZS-SVC-N-001 §4). Every write goes through the store, which
// already writes its own outbox_events row in the same transaction as
// the business write — this layer never publishes directly.
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

	"zoiko.io/reconciliation-engine-svc/internal/authz"
	"zoiko.io/reconciliation-engine-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-engine-svc/internal/envelope"
	"zoiko.io/reconciliation-engine-svc/internal/health"
	customMiddleware "zoiko.io/reconciliation-engine-svc/internal/middleware"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

const (
	ActionReconciliationManage = "RECONCILIATION_MANAGE"
	ActionReconciliationRead   = "RECONCILIATION_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/reconciliation", func(r chi.Router) {
		r.Post("/definitions", h.DefineReconciliation)
		r.Post("/runs", h.StartRun)
		r.Get("/runs/{runID}", h.GetRun)
		r.Post("/runs/{runID}:match", h.Match)
		r.Post("/runs/{runID}:raise-exception", h.RaiseException)
		r.Post("/runs/{runID}:reperform", h.Reperform)
		r.Post("/runs/{runID}:certify", h.Certify)
		r.Post("/runs/{runID}:supersede", h.SupersedeRun)
		r.Get("/runs/{runID}/populations", h.GetPopulationSnapshots)
		r.Get("/runs/{runID}/matches", h.GetMatchResults)
		r.Get("/runs/{runID}/exceptions", h.GetExceptions)
		r.Get("/runs/{runID}/certification", h.GetCertification)
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

// ── Commands ─────────────────────────────────────────────────────────────────

func (h *Handler) DefineReconciliation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	var req domain.DefineReconciliationRequest
	if _, err := h.readBody(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.DefineReconciliation(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) StartRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	var req domain.StartRunRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "StartRun", req.DefinitionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.StartRun(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) Match(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.MatchRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "Match", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.Match(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RaiseException(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.RaiseExceptionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RaiseException", runID+"|"+req.ItemID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RaiseException(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) Reperform(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principalID, "Reperform", runID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.Reperform(r.Context(), tenantID, runID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) Certify(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principalID, "Certify", runID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.Certify(r.Context(), tenantID, runID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) SupersedeRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.SupersedeRunRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "SupersedeRun", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.SupersedeRun(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationRead) {
		return
	}
	got, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetPopulationSnapshots(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationRead) {
		return
	}
	items, err := h.store.GetPopulationSnapshots(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.PopulationSnapshot{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetMatchResults(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationRead) {
		return
	}
	items, err := h.store.GetMatchResults(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.MatchResult{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetExceptions(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationRead) {
		return
	}
	items, err := h.store.GetExceptions(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.ReconciliationException{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionReconciliationRead) {
		return
	}
	got, err := h.store.GetCertification(r.Context(), tenantID, chi.URLParam(r, "runID"))
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
	case errors.Is(err, domain.ErrDefinitionNotFound), errors.Is(err, domain.ErrRunNotFound),
		errors.Is(err, domain.ErrItemNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrRunNotOpen), errors.Is(err, domain.ErrOpenExceptionsRemain),
		errors.Is(err, domain.ErrRunImmutable), errors.Is(err, domain.ErrExceptionAlreadyClosed),
		errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("reconciliation-engine-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
