// Package handler is the HTTP surface for data-ingestion-svc (DATA-01,
// ZS-SVC-N-001 §4). Every write goes through the store, which already
// writes its own outbox_events row in the same transaction as the
// business write — this layer never publishes directly, so a Kafka
// publish can never race ahead of (or fall behind) the fact it describes.
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

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/domain"
	svcenvelope "zoiko.io/data-ingestion-svc/internal/envelope"
	"zoiko.io/data-ingestion-svc/internal/health"
	customMiddleware "zoiko.io/data-ingestion-svc/internal/middleware"
	"zoiko.io/data-ingestion-svc/internal/store"
)

const (
	ActionIngestionManage = "DATA_INGESTION_MANAGE"
	ActionIngestionRead   = "DATA_INGESTION_READ"
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

	// /healthz stays OUTSIDE the tenant gate — see forecasting-svc's
	// identical comment: a blanket tenant requirement here would 401
	// every liveness/readiness probe.
	r.Get("/healthz", health.Handler())

	r.With(customMiddleware.TenantMiddleware).Route("/v1/data-ingestion", func(r chi.Router) {
		r.Post("/runs", h.StartIngestion)
		r.Get("/runs/{runID}", h.GetRun)
		r.Post("/runs/{runID}:close", h.CloseRun)
		r.Post("/runs/{runID}:commit", h.CommitBatch)
		r.Post("/runs/{runID}:quarantine", h.QuarantineBatch)
		r.Post("/runs/{runID}:replay", h.ReplayFromCheckpoint)
		r.Get("/runs/{runID}/quarantine-items", h.ListQuarantineItems)
		r.Get("/checkpoints/{sourceID}", h.GetCheckpoint)
	})

	return r
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification.
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
// idempotency claim's request_sha256 — the thing that tells a replayed
// key from a key reused for a genuinely different request apart), and
// decodes it into v. Handlers must use this instead of a fresh
// json.NewDecoder(r.Body) — the body can only be read once.
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

type startIngestionRequest struct {
	SourceID        string `json:"source_id"`
	ResidencyRegion string `json:"residency_region"`
	Classification  string `json:"classification"`
	Purpose         string `json:"purpose"`
}

func (h *Handler) StartIngestion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	var req startIngestionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run := &domain.IngestionRun{
		SourceID: req.SourceID, CreatedBy: principalID,
		ResidencyRegion: req.ResidencyRegion, Classification: req.Classification, Purpose: req.Purpose,
	}
	claim := h.idempotencyClaim(principalID, "StartIngestion", req.SourceID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.StartIngestion(r.Context(), tenantID, run, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) CommitBatch(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.CommitBatchRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.RunID = runID
	claim := h.idempotencyClaim(principalID, "CommitBatch", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CommitBatch(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) QuarantineBatch(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.QuarantineBatchRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.RunID = runID
	claim := h.idempotencyClaim(principalID, "QuarantineBatch", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.QuarantineBatch(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) ReplayFromCheckpoint(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.ReplayFromCheckpointRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.RunID = runID
	claim := h.idempotencyClaim(principalID, "ReplayFromCheckpoint", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ReplayFromCheckpoint(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

type closeRunRequest struct {
	FinalStatus string `json:"final_status"`
}

func (h *Handler) CloseRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req closeRunRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	status := domain.IngestionRunStatus(req.FinalStatus)
	if status != domain.IngestionRunCompleted && status != domain.IngestionRunFailed && status != domain.IngestionRunSuperseded {
		h.respondError(w, http.StatusBadRequest, "final_status must be Completed, Failed or Superseded")
		return
	}
	claim := h.idempotencyClaim(principalID, "CloseRun", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CloseRun(r.Context(), tenantID, domain.CloseRunRequest{RunID: runID}, status, principalID, claim)
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
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionRead) {
		return
	}
	got, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetCheckpoint(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionRead) {
		return
	}
	got, err := h.store.GetCheckpoint(r.Context(), tenantID, chi.URLParam(r, "sourceID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) ListQuarantineItems(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionRead) {
		return
	}
	items, err := h.store.ListQuarantineItems(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.QuarantineItem{}
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

// respondFromError maps a store-layer domain error to the right HTTP
// status — fail closed to 500 (logged) rather than guess for anything not
// explicitly named here.
func (h *Handler) respondFromError(w http.ResponseWriter, err error) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		h.respondJSON(w, http.StatusOK, map[string]string{"resource_id": replay.ResourceID})
		return
	}
	switch {
	case errors.Is(err, domain.ErrIngestionRunNotFound), errors.Is(err, domain.ErrSourceCheckpointNotFound),
		errors.Is(err, domain.ErrLandingObjectNotFound), errors.Is(err, domain.ErrQuarantineItemNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrIngestionRunInvalidState), errors.Is(err, domain.ErrRunAlreadyClosed),
		errors.Is(err, domain.ErrRunNotRunning):
		h.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrWrongResidencyRegion), errors.Is(err, domain.ErrSchemaIncompatible),
		errors.Is(err, domain.ErrDuplicateSourceEvent):
		h.respondError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("data-ingestion-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
