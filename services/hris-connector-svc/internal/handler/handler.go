package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/hris-connector-svc/internal/authz"
	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/events"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/hris", func(r chi.Router) {
		r.Post("/integrations", h.CreateIntegration)
		r.Get("/integrations", h.ListIntegrations)
		r.Get("/integrations/{id}", h.GetIntegrationByID)
		r.Post("/sync", h.TriggerSync)
		r.Get("/sync/jobs", h.ListSyncJobs)
	})
}

func (h *Handler) CreateIntegration(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.ProviderName == "" || req.ApiEndpoint == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, provider_name, and api_endpoint are required")
		return
	}

	integration := &domain.HrisIntegration{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		ProviderName:  req.ProviderName,
		ApiEndpoint:   req.ApiEndpoint,
		Status:        "ACTIVE",
	}

	if err := h.store.CreateIntegration(r.Context(), integration); err != nil {
		h.logger.Error("failed to create HRIS integration", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create HRIS integration")
		return
	}

	_ = h.publisher.Publish(r.Context(), "hris.integration.created", integration.IntegrationID, tenantID, integration)
	writeJSON(w, http.StatusCreated, integration)
}

func (h *Handler) GetIntegrationByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	integration, err := h.store.GetIntegrationByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrIntegrationNotFound) {
			writeError(w, http.StatusNotFound, "HRIS integration not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get HRIS integration")
		return
	}
	writeJSON(w, http.StatusOK, integration)
}

func (h *Handler) ListIntegrations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	integrations, err := h.store.ListIntegrations(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list HRIS integrations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"integrations": integrations,
		"total":        len(integrations),
	})
}

func (h *Handler) TriggerSync(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.TriggerSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IntegrationID == "" || req.SyncType == "" {
		writeError(w, http.StatusBadRequest, "integration_id and sync_type are required")
		return
	}

	if _, err := h.store.GetIntegrationByID(r.Context(), req.IntegrationID); err != nil {
		if errors.Is(err, domain.ErrIntegrationNotFound) {
			writeError(w, http.StatusNotFound, "HRIS integration not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify HRIS integration")
		return
	}

	job := &domain.SyncJob{
		IntegrationID: req.IntegrationID,
		TenantID:      tenantID,
		SyncType:      req.SyncType,
		RecordsSynced: 42,
		Status:        domain.SyncCompleted,
	}

	if err := h.store.CreateSyncJob(r.Context(), job); err != nil {
		h.logger.Error("failed to create sync job", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to trigger sync job")
		return
	}

	_ = h.publisher.Publish(r.Context(), "hris.sync.completed", job.JobID, tenantID, job)
	writeJSON(w, http.StatusCreated, job)
}

func (h *Handler) ListSyncJobs(w http.ResponseWriter, r *http.Request) {
	integrationID := r.URL.Query().Get("integration_id")
	jobs, err := h.store.ListSyncJobs(r.Context(), integrationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list HRIS sync jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":  jobs,
		"total": len(jobs),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
