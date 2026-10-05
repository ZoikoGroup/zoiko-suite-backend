// Package handler is the HTTP surface for analytical-data-platform-svc
// (DATA-04, ZS-SVC-N-001 §4). Every write goes through the store, which
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
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/analytical-data-platform-svc/internal/authz"
	"zoiko.io/analytical-data-platform-svc/internal/domain"
	svcenvelope "zoiko.io/analytical-data-platform-svc/internal/envelope"
	"zoiko.io/analytical-data-platform-svc/internal/health"
	customMiddleware "zoiko.io/analytical-data-platform-svc/internal/middleware"
	"zoiko.io/analytical-data-platform-svc/internal/store"
)

const (
	ActionADPManage = "ANALYTICAL_DATA_PLATFORM_MANAGE"
	ActionADPRead   = "ANALYTICAL_DATA_PLATFORM_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/analytical-data-platform", func(r chi.Router) {
		r.Post("/versions", h.CreateDatasetVersion)
		r.Get("/versions/{versionID}", h.GetDatasetVersion)
		r.Get("/versions/{versionID}/freshness", h.GetFreshness)
		r.Get("/versions/{versionID}/snapshot", h.GetLatestSnapshot)
		r.Get("/versions/{versionID}/partitions", h.GetPartitions)
		r.Get("/versions/{versionID}/certification", h.GetCertification)
		r.Post("/versions/{versionID}:publish", h.PublishDatasetVersion)
		r.Post("/versions/{versionID}:deprecate", h.DeprecateDataset)

		r.Post("/snapshots", h.BuildSnapshot)
		r.Post("/partitions", h.RebuildPartition)
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

func (h *Handler) CreateDatasetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPManage) {
		return
	}
	var req domain.CreateDatasetVersionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "CreateDatasetVersion", req.DatasetName, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CreateDatasetVersion(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) BuildSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPManage) {
		return
	}
	var req domain.BuildSnapshotRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "BuildSnapshot", req.VersionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.BuildSnapshot(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) RebuildPartition(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPManage) {
		return
	}
	var req domain.RebuildPartitionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RebuildPartition", req.VersionID+"|"+req.PartitionKey, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RebuildPartition(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) PublishDatasetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPManage) {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	claim := h.idempotencyClaim(principalID, "PublishDatasetVersion", versionID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.PublishDatasetVersion(r.Context(), tenantID, versionID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) DeprecateDataset(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPManage) {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	var req struct {
		Reason string `json:"reason"`
	}
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "DeprecateDataset", versionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.DeprecateDataset(r.Context(), tenantID, versionID, req.Reason, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetDatasetVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPRead) {
		return
	}
	got, err := h.store.GetDatasetVersion(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetLatestSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPRead) {
		return
	}
	got, err := h.store.GetLatestSnapshot(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetPartitions(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPRead) {
		return
	}
	items, err := h.store.GetPartitions(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.Partition{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPRead) {
		return
	}
	got, err := h.store.GetCertification(r.Context(), tenantID, chi.URLParam(r, "versionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetFreshness(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionADPRead) {
		return
	}
	asOf := time.Now().UTC()
	if s := r.URL.Query().Get("as_of"); s != "" {
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
			asOf = time.Unix(secs, 0).UTC()
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			asOf = t
		} else {
			h.respondError(w, http.StatusBadRequest, "as_of must be a unix timestamp or RFC3339 datetime")
			return
		}
	}
	got, err := h.store.GetFreshness(r.Context(), tenantID, chi.URLParam(r, "versionID"), asOf)
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
	case errors.Is(err, domain.ErrDatasetNotFound), errors.Is(err, domain.ErrDatasetVersionNotFound),
		errors.Is(err, domain.ErrSnapshotNotFound), errors.Is(err, domain.ErrNoSnapshotYet),
		errors.Is(err, domain.ErrCertificationNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrVersionNotValidated), errors.Is(err, domain.ErrVersionPublished),
		errors.Is(err, domain.ErrVersionNotDeprecatable), errors.Is(err, domain.ErrImmutableViolation),
		errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("analytical-data-platform-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
