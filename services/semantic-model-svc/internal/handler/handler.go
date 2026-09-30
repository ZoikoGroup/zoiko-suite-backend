// Package handler is the HTTP surface for semantic-model-svc (DATA-05,
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

	"zoiko.io/semantic-model-svc/internal/authz"
	"zoiko.io/semantic-model-svc/internal/domain"
	svcenvelope "zoiko.io/semantic-model-svc/internal/envelope"
	"zoiko.io/semantic-model-svc/internal/health"
	customMiddleware "zoiko.io/semantic-model-svc/internal/middleware"
	"zoiko.io/semantic-model-svc/internal/store"
)

const (
	ActionSemanticManage = "SEMANTIC_MODEL_MANAGE"
	ActionSemanticRead   = "SEMANTIC_MODEL_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/semantic-model", func(r chi.Router) {
		r.Post("/versions", h.CreateDraftVersion)
		r.Get("/versions/{versionID}", h.GetVersion)
		r.Post("/versions/{versionID}:publish", h.PublishSemanticVersion)
		r.Post("/versions/{versionID}:deprecate", h.DeprecateVersion)
		r.Get("/versions/{versionID}/metrics", h.GetMetricBindings)
		r.Get("/versions/{versionID}/dimensions", h.GetDimensionBindings)
		r.Get("/versions/{versionID}/calculation-plans", h.GetCalculationPlans)
		r.Get("/versions/{versionID}/calculation-plans/{metricKey}:validate", h.ValidateCalculationPlan)

		r.Post("/metric-bindings", h.AddMetricBinding)
		r.Post("/metric-bindings/{metricBindingID}:retire", h.RetireMetricBinding)
		r.Post("/dimension-bindings", h.AddDimensionBinding)
		r.Post("/calculation-plans", h.AddCalculationPlan)
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

func (h *Handler) CreateDraftVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	var req domain.CreateDraftVersionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "CreateDraftVersion", req.ModelName, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CreateDraftVersion(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) AddMetricBinding(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	var req domain.AddMetricBindingRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "AddMetricBinding", req.VersionID+"|"+req.MetricKey, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AddMetricBinding(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) AddDimensionBinding(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	var req domain.AddDimensionBindingRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "AddDimensionBinding", req.VersionID+"|"+req.DimensionKey, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AddDimensionBinding(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) AddCalculationPlan(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	var req domain.AddCalculationPlanRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "AddCalculationPlan", req.VersionID+"|"+req.MetricKey, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AddCalculationPlan(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) RetireMetricBinding(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	metricBindingID := chi.URLParam(r, "metricBindingID")
	claim := h.idempotencyClaim(principalID, "RetireMetricBinding", metricBindingID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.RetireMetricBinding(r.Context(), tenantID, metricBindingID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) PublishSemanticVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	claim := h.idempotencyClaim(principalID, "PublishSemanticVersion", versionID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.PublishSemanticVersion(r.Context(), tenantID, versionID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) DeprecateVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticManage) {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	claim := h.idempotencyClaim(principalID, "DeprecateVersion", versionID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.DeprecateVersion(r.Context(), tenantID, versionID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticRead) {
		return
	}
	got, err := h.store.GetVersion(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetMetricBindings(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticRead) {
		return
	}
	items, err := h.store.GetMetricBindings(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.MetricBinding{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetDimensionBindings(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticRead) {
		return
	}
	items, err := h.store.GetDimensionBindings(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.DimensionBinding{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetCalculationPlans(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticRead) {
		return
	}
	items, err := h.store.GetCalculationPlans(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.CalculationPlan{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) ValidateCalculationPlan(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionSemanticRead) {
		return
	}
	got, err := h.store.ValidateCalculationPlan(r.Context(), tenantID, chi.URLParam(r, "versionID"), chi.URLParam(r, "metricKey"))
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
	case errors.Is(err, domain.ErrModelNotFound), errors.Is(err, domain.ErrVersionNotFound),
		errors.Is(err, domain.ErrMetricBindingNotFound), errors.Is(err, domain.ErrCalcPlanNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrVersionNotDraft), errors.Is(err, domain.ErrVersionPublished),
		errors.Is(err, domain.ErrVersionAlreadyRetired), errors.Is(err, domain.ErrNoBindings),
		errors.Is(err, domain.ErrMetricCollision), errors.Is(err, domain.ErrCalcPlanInvalid),
		errors.Is(err, domain.ErrImmutableViolation), errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("semantic-model-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
