// Package handler is the HTTP surface for document-extraction-svc
// (AI-01, ZS-SVC-N-001 §4). Every write goes through the store, which
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

	"zoiko.io/document-extraction-svc/internal/authz"
	"zoiko.io/document-extraction-svc/internal/domain"
	svcenvelope "zoiko.io/document-extraction-svc/internal/envelope"
	"zoiko.io/document-extraction-svc/internal/health"
	customMiddleware "zoiko.io/document-extraction-svc/internal/middleware"
	"zoiko.io/document-extraction-svc/internal/store"
)

const (
	ActionExtractionManage = "DOCUMENT_EXTRACTION_MANAGE"
	ActionExtractionRead   = "DOCUMENT_EXTRACTION_READ"
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

	r.With(customMiddleware.TenantMiddleware).Route("/v1/document-extraction", func(r chi.Router) {
		r.Post("/jobs", h.ExtractDocument)
		r.Get("/jobs/{jobID}", h.GetJob)
		r.Post("/jobs/{jobID}:reprocess", h.ReprocessWithVersion)
		r.Get("/jobs/{jobID}/candidates", h.GetCandidates)
		r.Get("/jobs/{jobID}/invocation", h.GetModelInvocation)
		r.Post("/candidates/{candidateID}:accept", h.AcceptCandidate)
		r.Post("/candidates/{candidateID}:reject", h.RejectCandidate)
		r.Get("/candidates/{candidateID}/evidence-span", h.GetEvidenceSpan)
		r.Get("/candidates/{candidateID}/decisions", h.GetDecisions)
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

func (h *Handler) ExtractDocument(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionManage) {
		return
	}
	var req domain.ExtractDocumentRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "ExtractDocument", req.DocumentRef, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ExtractDocument(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) ReprocessWithVersion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionManage) {
		return
	}
	jobID := chi.URLParam(r, "jobID")
	var req domain.ReprocessWithVersionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "ReprocessWithVersion", jobID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ReprocessWithVersion(r.Context(), tenantID, jobID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) AcceptCandidate(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionManage) {
		return
	}
	candidateID := chi.URLParam(r, "candidateID")
	claim := h.idempotencyClaim(principalID, "AcceptCandidate", candidateID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.AcceptCandidate(r.Context(), tenantID, candidateID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RejectCandidate(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionManage) {
		return
	}
	candidateID := chi.URLParam(r, "candidateID")
	var req domain.RejectCandidateRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RejectCandidate", candidateID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RejectCandidate(r.Context(), tenantID, candidateID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionRead) {
		return
	}
	got, err := h.store.GetJob(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetCandidates(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionRead) {
		return
	}
	items, err := h.store.GetCandidates(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.ExtractionCandidate{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetModelInvocation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionRead) {
		return
	}
	got, err := h.store.GetModelInvocation(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetEvidenceSpan(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionRead) {
		return
	}
	got, err := h.store.GetEvidenceSpan(r.Context(), tenantID, chi.URLParam(r, "candidateID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetDecisions(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionExtractionRead) {
		return
	}
	items, err := h.store.GetDecisions(r.Context(), tenantID, chi.URLParam(r, "candidateID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.ExtractionDecision{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
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
	case errors.Is(err, domain.ErrJobNotFound), errors.Is(err, domain.ErrCandidateNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrCandidateNotPending), errors.Is(err, domain.ErrJobNotFinal),
		errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("document-extraction-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
