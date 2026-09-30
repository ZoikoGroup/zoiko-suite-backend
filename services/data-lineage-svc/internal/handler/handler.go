// Package handler is the HTTP surface for data-lineage-svc (DATA-03,
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
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/data-lineage-svc/internal/authz"
	"zoiko.io/data-lineage-svc/internal/domain"
	svcenvelope "zoiko.io/data-lineage-svc/internal/envelope"
	"zoiko.io/data-lineage-svc/internal/health"
	customMiddleware "zoiko.io/data-lineage-svc/internal/middleware"
	"zoiko.io/data-lineage-svc/internal/store"
)

const (
	ActionLineageManage = "DATA_LINEAGE_MANAGE"
	ActionLineageRead   = "DATA_LINEAGE_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/data-lineage", func(r chi.Router) {
		r.Post("/derivations", h.RecordDerivation)
		r.Post("/edges/{edgeID}:supersede", h.SupersedeLineage)
		r.Post("/evidence", h.AttachEvidence)
		r.Post("/manifests/{entityID}:seal", h.SealManifest)
		r.Get("/entities/{entityID}", h.GetEntity)
		r.Get("/entities/{entityID}/upstream", h.GetUpstreamLineage)
		r.Get("/entities/{entityID}/downstream", h.GetDownstreamImpact)
		r.Get("/entities/{entityID}/as-of", h.GetAsOfLineage)
		r.Get("/entities/{entityID}/source-path", h.GetSourceToReportPath)
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

// readBody reads the whole request body once, hashes it (for the
// idempotency claim's request_sha256), and decodes it into v.
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

func (h *Handler) RecordDerivation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageManage) {
		return
	}
	var req domain.RecordDerivationRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RecordDerivation",
		req.SourceEntity.ExternalRef+"->"+req.DerivedEntity.ExternalRef, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RecordDerivation(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) SupersedeLineage(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageManage) {
		return
	}
	edgeID := chi.URLParam(r, "edgeID")
	var req domain.SupersedeLineageRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.EdgeID = edgeID
	claim := h.idempotencyClaim(principalID, "SupersedeLineage", edgeID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.SupersedeLineage(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) AttachEvidence(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageManage) {
		return
	}
	var req domain.AttachEvidenceRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "AttachEvidence", req.Entity.ExternalRef+"|"+req.EvidenceRef, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AttachEvidence(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) SealManifest(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageManage) {
		return
	}
	entityID := chi.URLParam(r, "entityID")
	claim := h.idempotencyClaim(principalID, "SealManifest", entityID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.SealManifest(r.Context(), tenantID, entityID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetEntity(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	got, err := h.store.GetEntity(r.Context(), tenantID, chi.URLParam(r, "entityID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetUpstreamLineage(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	got, err := h.store.GetUpstreamLineage(r.Context(), tenantID, chi.URLParam(r, "entityID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetDownstreamImpact(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	got, err := h.store.GetDownstreamImpact(r.Context(), tenantID, chi.URLParam(r, "entityID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetAsOfLineage(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	asOfParam := r.URL.Query().Get("as_of")
	if asOfParam == "" {
		h.respondError(w, http.StatusBadRequest, "as_of query parameter (unix seconds or RFC3339) is required")
		return
	}
	asOf, err := parseAsOf(asOfParam)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "as_of must be a unix timestamp or RFC3339 datetime")
		return
	}
	got, err := h.store.GetAsOfLineage(r.Context(), tenantID, chi.URLParam(r, "entityID"), asOf)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func parseAsOf(s string) (time.Time, error) {
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Parse(time.RFC3339, s)
}

func (h *Handler) GetSourceToReportPath(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	roots, err := h.store.GetSourceToReportPath(r.Context(), tenantID, chi.URLParam(r, "entityID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if roots == nil {
		roots = []domain.LineageEntity{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": roots, "count": len(roots)})
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
	case errors.Is(err, domain.ErrLineageEntityNotFound), errors.Is(err, domain.ErrLineageEdgeNotFound),
		errors.Is(err, domain.ErrProvenanceManifestNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrLineageEdgeAlreadySuperseded), errors.Is(err, domain.ErrMissingTransformationVersion),
		errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("data-lineage-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
