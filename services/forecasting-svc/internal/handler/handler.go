package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/health"
	customMiddleware "zoiko.io/forecasting-svc/internal/middleware"
	"zoiko.io/forecasting-svc/internal/store"
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

	r.Route("/v1/forecasts", func(r chi.Router) {
		r.Post("/generate", h.GenerateForecast)
		r.Get("/", h.ListForecasts)
		r.Get("/{id}", h.GetForecastByID)
		r.Post("/{id}/recalculate", h.RecalculateForecast)
		r.Delete("/{id}", h.ArchiveForecast)
	})

	return r
}

func (h *Handler) GenerateForecast(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	var req domain.GenerateForecastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := req.Validate(); err != nil {
		h.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	model := &domain.ForecastModel{
		LegalEntityID:       req.LegalEntityID,
		ModelName:           req.ModelName,
		Domain:              req.Domain,
		ScenarioType:        req.ScenarioType,
		AlgorithmType:       req.AlgorithmType,
		Granularity:         req.Granularity,
		HorizonPeriods:      req.HorizonPeriods,
		HistoricalStartDate: req.HistoricalStartDate,
		Status:              "ACTIVE",
		ConfidenceLevel:     95.0,
		Metadata:            req.Metadata,
	}

	projections := domain.ComputeProjections(&req, "", tenantID)

	if err := h.store.CreateForecast(r.Context(), tenantID, model, projections); err != nil {
		h.logger.Error("failed to create forecast model", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to generate forecast")
		return
	}

	_ = h.publisher.Publish(r.Context(), "forecast.generated", tenantID, model)

	h.respondJSON(w, http.StatusCreated, model)
}

func (h *Handler) ListForecasts(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	domainName := r.URL.Query().Get("domain")
	scenario := r.URL.Query().Get("scenario")

	forecasts, err := h.store.ListForecasts(r.Context(), tenantID, legalEntityID, domainName, scenario)
	if err != nil {
		h.logger.Error("failed to list forecasts", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to query forecasts")
		return
	}

	if forecasts == nil {
		forecasts = []domain.ForecastModel{}
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":  forecasts,
		"count": len(forecasts),
	})
}

func (h *Handler) GetForecastByID(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	forecast, err := h.store.GetForecastByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "forecast model not found")
		return
	}

	h.respondJSON(w, http.StatusOK, forecast)
}

func (h *Handler) RecalculateForecast(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	var req domain.RecalculateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	updated, err := h.store.RecalculateForecast(r.Context(), tenantID, id, req.GrowthRateAdjustment, req.ScenarioType)
	if err != nil {
		h.respondError(w, http.StatusNotFound, err.Error())
		return
	}

	_ = h.publisher.Publish(r.Context(), "forecast.updated", tenantID, updated)

	h.respondJSON(w, http.StatusOK, updated)
}

func (h *Handler) ArchiveForecast(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	if err := h.store.ArchiveForecast(r.Context(), tenantID, id); err != nil {
		h.respondError(w, http.StatusNotFound, "forecast model not found")
		return
	}

	_ = h.publisher.Publish(r.Context(), "forecast.archived", tenantID, map[string]string{"id": id, "status": "ARCHIVED"})

	h.respondJSON(w, http.StatusOK, map[string]string{
		"message": "forecast model archived successfully",
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
