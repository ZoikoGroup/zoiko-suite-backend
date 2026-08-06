package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/events"
	"zoiko.io/migration-integrity-svc/internal/health"
	"zoiko.io/migration-integrity-svc/internal/middleware"
	"zoiko.io/migration-integrity-svc/internal/store"
)

const (
	principalIDHeader = "X-Principal-Id"

	ActionMigrationJobValidate = "MIGRATION_JOB_VALIDATE"
	ActionMigrationJobArchive  = "MIGRATION_JOB_ARCHIVE"
	ActionAuditEntryRemediate  = "AUDIT_ENTRY_REMEDIATE"
)

type Handler struct {
	store     store.Store
	publisher *events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func NewHandler(s store.Store, p *events.Publisher, a *authz.Client, l *zap.Logger) *Handler {
	return &Handler{store: s, publisher: p, authz: a, logger: l}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	r.Use(middleware.TenantMiddleware)

	r.Get("/healthz", health.Handler())

	r.Route("/v1/migrations", func(r chi.Router) {
		r.Post("/validate", h.ValidateMigration)
		r.Get("/", h.ListJobs)
		r.Get("/{id}", h.GetJobByID)
		r.Post("/{id}/audit/{entryId}/remediate", h.RemediateEntry)
		r.Delete("/{id}", h.ArchiveJob)
	})

	return r
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (h *Handler) ValidateMigration(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.ValidateMigrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	if !h.checkAllowed(w, r, principalID, req.LegalEntityID, ActionMigrationJobValidate) {
		return
	}

	jobID := uuid.New().String()
	now := time.Now()

	checks, entries, validCount, invalidCount, score := domain.PerformIntegrityValidation(&req, jobID, tenantID)

	status := domain.JobStatusCompleted
	if invalidCount == len(req.Records) {
		status = domain.JobStatusFailed
	}

	job := &domain.MigrationJob{
		ID:                  jobID,
		LegalEntityID:       req.LegalEntityID,
		MigrationName:       req.MigrationName,
		SourceSystem:        req.SourceSystem,
		TargetService:       req.TargetService,
		TotalRecordsCount:   len(req.Records),
		ValidRecordsCount:   validCount,
		InvalidRecordsCount: invalidCount,
		IntegrityScore:      score,
		Status:              status,
		StartedAt:           &now,
		CompletedAt:         &now,
	}

	if err := h.store.CreateJob(r.Context(), tenantID, job, checks, entries); err != nil {
		h.logger.Error("failed to save migration job", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to persist migration validation results")
		return
	}

	_ = h.publisher.Publish(r.Context(), "migration.integrity_validated", tenantID, map[string]interface{}{
		"job_id":          job.ID,
		"integrity_score": score,
		"status":          string(status),
	})

	if invalidCount > 0 {
		_ = h.publisher.Publish(r.Context(), "migration.integrity_violations_detected", tenantID, map[string]interface{}{
			"job_id":        job.ID,
			"invalid_count": invalidCount,
		})
	}

	h.okJSON(w, http.StatusCreated, job)
}

func (h *Handler) GetJobByID(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}
	h.okJSON(w, http.StatusOK, job)
}

func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")

	jobs, err := h.store.ListJobs(r.Context(), tenantID, legalEntityID, status)
	if err != nil {
		h.logger.Error("failed to list migration jobs", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to query migration jobs")
		return
	}
	if jobs == nil {
		jobs = []domain.MigrationJob{}
	}
	h.okJSON(w, http.StatusOK, map[string]interface{}{"data": jobs, "count": len(jobs)})
}

func (h *Handler) RemediateEntry(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	entryID := chi.URLParam(r, "entryId")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}

	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionAuditEntryRemediate) {
		return
	}

	var req domain.RemediateRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	entry, err := h.store.RemediateEntry(r.Context(), tenantID, jobID, entryID, req.Notes)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, err.Error())
		return
	}

	_ = h.publisher.Publish(r.Context(), "migration.audit_entry_remediated", tenantID, entry)
	h.okJSON(w, http.StatusOK, entry)
}

func (h *Handler) ArchiveJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}

	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionMigrationJobArchive) {
		return
	}

	if err := h.store.ArchiveJob(r.Context(), tenantID, id); err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}

	_ = h.publisher.Publish(r.Context(), "migration.job_archived", tenantID, map[string]string{"job_id": id})
	h.okJSON(w, http.StatusOK, map[string]string{"message": "migration job archived", "id": id})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) okJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}

// requirePrincipal extracts the caller's principal ID from the
// X-Principal-Id request header. If missing, it writes a 401 response and
// returns ok=false so the caller can abort the request.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		h.errJSON(w, http.StatusUnauthorized, "missing "+principalIDHeader+" header")
		return "", false
	}
	return principalID, true
}

// checkAllowed enforces a fail-closed authorization check against
// authorization-svc before any write mutation. It writes the appropriate
// error response and returns false when the action must not proceed.
func (h *Handler) checkAllowed(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			h.errJSON(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.logger.Error("authorization check failed", zap.Error(err))
		h.errJSON(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}
