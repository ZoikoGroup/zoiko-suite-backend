// Package handler is the HTTP surface for data-quality-svc (DATA-02,
// ZS-SVC-N-001 §4). Every write goes through the store, which already
// writes its own outbox_events row in the same transaction as the
// business write — this layer never publishes directly.
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

	"zoiko.io/data-quality-svc/internal/authz"
	"zoiko.io/data-quality-svc/internal/domain"
	svcenvelope "zoiko.io/data-quality-svc/internal/envelope"
	"zoiko.io/data-quality-svc/internal/health"
	customMiddleware "zoiko.io/data-quality-svc/internal/middleware"
	"zoiko.io/data-quality-svc/internal/store"
)

const (
	ActionDQManage = "DATA_QUALITY_MANAGE"
	ActionDQRead   = "DATA_QUALITY_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/data-quality", func(r chi.Router) {
		r.Post("/rulesets/{name}:publish", h.PublishRuleSetVersion)
		r.Get("/ruleset-versions/{versionID}", h.GetRuleSetVersion)

		r.Post("/runs", h.RunDQ)
		r.Get("/runs/{runID}", h.GetRun)
		r.Get("/runs/{runID}/results", h.GetResults)
		r.Get("/runs/{runID}/issues", h.GetIssues)
		r.Post("/runs/{runID}:reperform", h.Reperform)
		r.Post("/runs/{runID}:certify", h.CertifyDQ)
		r.Get("/runs/{runID}/certification", h.GetCertification)

		r.Post("/issues", h.RaiseIssue)
		r.Post("/issues/{issueID}:assign", h.AssignIssue)
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

type publishRuleSetVersionRequest struct {
	Rules []domain.RuleDefinition `json:"rules"`
}

func (h *Handler) PublishRuleSetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	name := chi.URLParam(r, "name")
	var req publishRuleSetVersionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "PublishRuleSetVersion", name, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.PublishRuleSetVersion(r.Context(), tenantID, name, req.Rules, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) RunDQ(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	var req domain.RunDQRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RunDQ", req.PopulationRef, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RunDQ(r.Context(), tenantID, req, principalID, claim)
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
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.RunDQRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "Reperform", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.Reperform(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) CertifyDQ(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principalID, "CertifyDQ", runID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.CertifyDQ(r.Context(), tenantID, runID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) RaiseIssue(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	var req domain.RaiseIssueRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RaiseIssue", req.ResultID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RaiseIssue(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) AssignIssue(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQManage) {
		return
	}
	issueID := chi.URLParam(r, "issueID")
	var req domain.AssignIssueRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.IssueID = issueID
	claim := h.idempotencyClaim(principalID, "AssignIssue", issueID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AssignIssue(r.Context(), tenantID, req, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetRuleSetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQRead) {
		return
	}
	got, err := h.store.GetRuleSetVersion(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQRead) {
		return
	}
	got, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetResults(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQRead) {
		return
	}
	items, err := h.store.GetResults(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.DQResult{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetIssues(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQRead) {
		return
	}
	items, err := h.store.GetIssues(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.DQIssue{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionDQRead) {
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
	case errors.Is(err, domain.ErrRuleSetNotFound), errors.Is(err, domain.ErrRuleSetVersionNotFound),
		errors.Is(err, domain.ErrRunNotFound), errors.Is(err, domain.ErrResultNotFound),
		errors.Is(err, domain.ErrIssueNotFound), errors.Is(err, domain.ErrCertificationNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrRunHasOpenIssues), errors.Is(err, domain.ErrRunAlreadyCertified),
		errors.Is(err, domain.ErrRunNotInExceptionsOpen), errors.Is(err, domain.ErrIssueAlreadyAssigned),
		errors.Is(err, domain.ErrImmutableViolation), errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("data-quality-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
