package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/events"
	"zoiko.io/reconciliation-intelligence-svc/internal/health"
	customMiddleware "zoiko.io/reconciliation-intelligence-svc/internal/middleware"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher *events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func NewHandler(s store.Store, p *events.Publisher, a *authz.Client, l *zap.Logger) *Handler {
	return &Handler{
		store:     s,
		publisher: p,
		authz:     a,
		logger:    l,
	}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(customMiddleware.TenantMiddleware)

	r.Get("/healthz", health.Handler())

	r.Route("/v1/reconciliations", func(r chi.Router) {
		r.Post("/analyze", h.AnalyzeReconciliation)
		r.Get("/", h.ListJobs)
		r.Get("/{id}", h.GetJobByID)
		r.Post("/{id}/resolutions/{itemId}/apply", h.ApplyResolution)
		r.Delete("/{id}", h.ArchiveJob)
	})

	return r
}

func (h *Handler) AnalyzeReconciliation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	var req domain.AnalyzeReconciliationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := req.Validate(); err != nil {
		h.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	matched, unmatched, rate, items := domain.PerformIntelligentReconciliation(&req, "", tenantID)

	job := &domain.ReconciliationJob{
		LegalEntityID:       req.LegalEntityID,
		JobName:             req.JobName,
		SourceSystemA:       req.SourceSystemA,
		SourceSystemB:       req.SourceSystemB,
		TotalProcessedCount: len(req.TransactionsA),
		MatchedCount:        matched,
		UnmatchedCount:      unmatched,
		ReconciliationRate:  rate,
		Status:              "COMPLETED",
	}

	if err := h.store.CreateJob(r.Context(), tenantID, job, items); err != nil {
		h.logger.Error("failed to save reconciliation job", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to run reconciliation analysis")
		return
	}

	_ = h.publisher.Publish(r.Context(), "reconciliation.analyzed", tenantID, job)

	if unmatched > 0 {
		_ = h.publisher.Publish(r.Context(), "reconciliation.unmatched_flagged", tenantID, map[string]interface{}{
			"job_id":          job.ID,
			"unmatched_count": unmatched,
			"rate":            rate,
		})
	}

	h.respondJSON(w, http.StatusCreated, job)
}

func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	sourceA := r.URL.Query().Get("source_system_a")
	status := r.URL.Query().Get("status")

	jobs, err := h.store.ListJobs(r.Context(), tenantID, legalEntityID, sourceA, status)
	if err != nil {
		h.logger.Error("failed to list reconciliation jobs", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to query reconciliation jobs")
		return
	}

	if jobs == nil {
		jobs = []domain.ReconciliationJob{}
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":  jobs,
		"count": len(jobs),
	})
}

func (h *Handler) GetJobByID(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	h.respondJSON(w, http.StatusOK, job)
}

func (h *Handler) ApplyResolution(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	itemID := chi.URLParam(r, "itemId")

	var req domain.ApplyResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ResolutionStatus == "" {
		req.ResolutionStatus = domain.StatusApproved
	}

	item, err := h.store.ApplyResolution(r.Context(), tenantID, jobID, itemID, req.ResolutionStatus, req.ResolutionNotes)
	if err != nil {
		h.respondError(w, http.StatusNotFound, err.Error())
		return
	}

	_ = h.publisher.Publish(r.Context(), "reconciliation.resolution_recommended", tenantID, item)

	h.respondJSON(w, http.StatusOK, item)
}

func (h *Handler) ArchiveJob(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	if err := h.store.ArchiveJob(r.Context(), tenantID, id); err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	h.respondJSON(w, http.StatusOK, map[string]string{
		"message": "reconciliation job archived successfully",
		"id":      id,
	})
}

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) respondError(w http.ResponseWriter, code int, message string) {
	h.respondJSON(w, code, map[string]string{"error": message})
}
