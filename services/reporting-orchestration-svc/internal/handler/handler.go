package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/events"
	"zoiko.io/reporting-orchestration-svc/internal/health"
	"zoiko.io/reporting-orchestration-svc/internal/middleware"
	"zoiko.io/reporting-orchestration-svc/internal/store"
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

	r.Route("/v1/reports", func(r chi.Router) {
		// Report Definitions
		r.Post("/definitions", h.CreateDefinition)
		r.Get("/definitions", h.ListDefinitions)
		r.Get("/definitions/{id}", h.GetDefinitionByID)
		r.Patch("/definitions/{id}/status", h.UpdateDefinitionStatus)

		// Report Runs
		r.Post("/definitions/{id}/runs", h.TriggerRun)
		r.Get("/runs", h.ListRuns)
		r.Get("/runs/{runId}", h.GetRunByID)
	})

	return r
}

// ─── Definition Handlers ──────────────────────────────────────────────────────

func (h *Handler) CreateDefinition(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateDefinitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	def := &domain.ReportDefinition{
		LegalEntityID: req.LegalEntityID,
		ReportName:    req.ReportName,
		ReportType:    req.ReportType,
		OutputFormat:  req.OutputFormat,
		DataSources:   req.DataSources,
		ScheduleCron:  req.ScheduleCron,
		IsScheduled:   req.IsScheduled,
	}

	if err := h.store.CreateDefinition(r.Context(), tenantID, def); err != nil {
		h.logger.Error("failed to create report definition", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to create report definition")
		return
	}

	_ = h.publisher.Publish(r.Context(), "report.definition_created", tenantID, def)
	h.okJSON(w, http.StatusCreated, def)
}

func (h *Handler) GetDefinitionByID(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	def, err := h.store.GetDefinitionByID(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "report definition not found")
		return
	}
	h.okJSON(w, http.StatusOK, def)
}

func (h *Handler) ListDefinitions(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	reportType := r.URL.Query().Get("report_type")

	defs, err := h.store.ListDefinitions(r.Context(), tenantID, legalEntityID, reportType)
	if err != nil {
		h.logger.Error("failed to list definitions", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to query definitions")
		return
	}
	if defs == nil {
		defs = []domain.ReportDefinition{}
	}
	h.okJSON(w, http.StatusOK, map[string]interface{}{"data": defs, "count": len(defs)})
}

func (h *Handler) UpdateDefinitionStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	var body struct {
		Status domain.DefinitionStatus `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Status == "" {
		h.errJSON(w, http.StatusBadRequest, "status is required")
		return
	}

	if err := h.store.UpdateDefinitionStatus(r.Context(), tenantID, id, body.Status); err != nil {
		h.errJSON(w, http.StatusNotFound, "report definition not found")
		return
	}

	_ = h.publisher.Publish(r.Context(), "report.definition_status_updated", tenantID, map[string]string{"id": id, "status": string(body.Status)})
	h.okJSON(w, http.StatusOK, map[string]string{"message": "status updated", "id": id, "status": string(body.Status)})
}

// ─── Run Handlers ─────────────────────────────────────────────────────────────

func (h *Handler) TriggerRun(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	defID := chi.URLParam(r, "id")

	// Validate definition exists
	def, err := h.store.GetDefinitionByID(r.Context(), tenantID, defID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "report definition not found")
		return
	}

	var req domain.TriggerRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.TriggeredBy = domain.TriggerManual
	}
	if req.TriggeredBy == "" {
		req.TriggeredBy = domain.TriggerManual
	}

	run := &domain.ReportRun{
		DefinitionID: defID,
		TriggeredBy:  req.TriggeredBy,
		PeriodStart:  req.PeriodStart,
		PeriodEnd:    req.PeriodEnd,
		Status:       domain.RunStatusPending,
	}

	if err := h.store.CreateRun(r.Context(), tenantID, run); err != nil {
		h.logger.Error("failed to create report run", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to trigger report run")
		return
	}

	// Orchestrate: simulate cross-service aggregation and run completion
	domain.OrchestratReportRun(def, run)
	_ = h.store.UpdateRun(r.Context(), tenantID, run)

	_ = h.publisher.Publish(r.Context(), "report.run_completed", tenantID, run)
	h.okJSON(w, http.StatusCreated, run)
}

func (h *Handler) GetRunByID(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	runID := chi.URLParam(r, "runId")

	run, err := h.store.GetRunByID(r.Context(), tenantID, runID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "report run not found")
		return
	}
	h.okJSON(w, http.StatusOK, run)
}

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	defID := r.URL.Query().Get("definition_id")
	status := r.URL.Query().Get("status")

	runs, err := h.store.ListRuns(r.Context(), tenantID, defID, status)
	if err != nil {
		h.logger.Error("failed to list runs", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to query runs")
		return
	}
	if runs == nil {
		runs = []domain.ReportRun{}
	}
	h.okJSON(w, http.StatusOK, map[string]interface{}{"data": runs, "count": len(runs)})
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
