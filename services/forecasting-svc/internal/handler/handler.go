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
	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/domain"
	svcenvelope "zoiko.io/forecasting-svc/internal/envelope"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/health"
	customMiddleware "zoiko.io/forecasting-svc/internal/middleware"
	"zoiko.io/forecasting-svc/internal/store"
)

const (
	FORECAST_CREATE      = "FORECAST_CREATE"
	FORECAST_RECALCULATE = "FORECAST_RECALCULATE"
	FORECAST_ARCHIVE     = "FORECAST_ARCHIVE"

	// AI-05 governed advisory layer actions.
	FORECAST_ASSIST_MANAGE = "FORECAST_ASSIST_MANAGE"
	FORECAST_ASSIST_READ   = "FORECAST_ASSIST_READ"
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

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// /healthz stays OUTSIDE the tenant gate. Liveness and readiness
	// probes carry no tenant, so a blanket tenant requirement would 401
	// every probe and the orchestrator would restart the container in a
	// loop — a security fix that takes the service down.
	r.Get("/healthz", health.Handler())

	// Everything below requires a gateway-verified tenant. Applied via
	// With() on the route subtree rather than as a path exemption inside
	// the middleware: comparing r.URL.Path to skip an auth gate is a
	// classic bypass source (traversal, trailing slash, case). The route
	// tree is the source of truth for what is and is not protected.
	r.With(customMiddleware.TenantMiddleware).Route("/v1/forecasts", func(r chi.Router) {
		r.Post("/generate", h.GenerateForecast)
		r.Get("/", h.ListForecasts)
		r.Get("/{id}", h.GetForecastByID)
		r.Post("/{id}/recalculate", h.RecalculateForecast)
		r.Delete("/{id}", h.ArchiveForecast)
	})

	// AI-05 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8) — additive,
	// lives alongside the forecasting-engine surface above under its own path.
	r.With(customMiddleware.TenantMiddleware).Route("/v1/forecast-assist", func(r chi.Router) {
		r.Post("/model-releases", h.RegisterForecastModelRelease)
		r.Post("/suggest-drivers", h.SuggestDrivers)
		r.Post("/suggest-range", h.SuggestRange)
		r.Post("/generate-narrative", h.GenerateNarrative)
		r.Post("/jobs/{jobID}:start-review", h.StartPlannerReview)
		r.Post("/jobs/{jobID}:accept", h.AcceptForecastSuggestion)
		r.Post("/jobs/{jobID}:reject", h.RejectForecastSuggestion)
		r.Get("/jobs/{jobID}", h.GetForecastAssistJob)
		r.Get("/jobs/{jobID}/drivers", h.GetSuggestedDrivers)
		r.Get("/jobs/{jobID}/ranges", h.GetSuggestedRanges)
		r.Get("/jobs/{jobID}/evidence", h.GetEvidenceReferences)
		r.Get("/jobs/{jobID}/decision", h.GetPlannerDecision)
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, FORECAST_CREATE); err != nil {
		h.writeAuthzErr(w, err)
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

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "forecast.generated", SubjectID: model.ID, TenantID: tenantID,
		LegalEntityID: model.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: model,
	})

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

	existing, err := h.store.GetForecastByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "forecast model not found")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, FORECAST_RECALCULATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.RecalculateForecast(r.Context(), tenantID, id, req.GrowthRateAdjustment, req.ScenarioType)
	if err != nil {
		h.respondError(w, http.StatusNotFound, err.Error())
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "forecast.updated", SubjectID: id, TenantID: tenantID,
		LegalEntityID: updated.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})

	h.respondJSON(w, http.StatusOK, updated)
}

func (h *Handler) ArchiveForecast(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	existing, err := h.store.GetForecastByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "forecast model not found")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, FORECAST_ARCHIVE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ArchiveForecast(r.Context(), tenantID, id); err != nil {
		h.respondError(w, http.StatusNotFound, "forecast model not found")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "forecast.archived", SubjectID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
		Payload:       map[string]string{"id": id, "status": "ARCHIVED"},
	})

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

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.respondError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		h.respondError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	h.respondError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8) — additive.
// ─────────────────────────────────────────────────────────────────────────────

func assistClaim(principalID, operation, resourceID, requestSHA256, key string) domain.AssistIdempotencyClaim {
	return domain.AssistIdempotencyClaim{
		OwnerScope: domain.AssistSellerScope, PrincipalID: principalID, Key: key,
		Operation: operation, RequestSHA256: requestSHA256, ResourceID: resourceID,
	}
}

func readAssistBody(r *http.Request, v interface{}) (string, error) {
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

func (h *Handler) RegisterForecastModelRelease(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.RegisterForecastModelReleaseRequest
	if _, err := readAssistBody(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.RegisterForecastModelRelease(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) SuggestDrivers(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.SuggestDriversRequest
	reqHash, err := readAssistBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := assistClaim(principalID, "SuggestDrivers", req.JobID+"|"+req.PlanVersion, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.SuggestDrivers(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) SuggestRange(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.SuggestRangeRequest
	reqHash, err := readAssistBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := assistClaim(principalID, "SuggestRange", req.JobID+"|"+req.PlanVersion, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.SuggestRange(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) GenerateNarrative(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.GenerateNarrativeRequest
	reqHash, err := readAssistBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := assistClaim(principalID, "GenerateNarrative", req.JobID+"|"+req.PlanVersion, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.GenerateNarrative(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) StartPlannerReview(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	jobID := chi.URLParam(r, "jobID")
	claim := assistClaim(principalID, "StartPlannerReview", jobID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.StartPlannerReview(r.Context(), tenantID, jobID, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) AcceptForecastSuggestion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	jobID := chi.URLParam(r, "jobID")
	claim := assistClaim(principalID, "AcceptSuggestion", jobID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.AcceptSuggestion(r.Context(), tenantID, jobID, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RejectForecastSuggestion(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	jobID := chi.URLParam(r, "jobID")
	var req domain.RejectForecastSuggestionRequest
	reqHash, err := readAssistBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := assistClaim(principalID, "RejectSuggestion", jobID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RejectSuggestion(r.Context(), tenantID, jobID, req, principalID, claim)
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetForecastAssistJob(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	got, err := h.store.GetForecastAssistJob(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetSuggestedDrivers(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	items, err := h.store.GetSuggestedDrivers(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	if items == nil {
		items = []domain.SuggestedDriver{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetSuggestedRanges(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	items, err := h.store.GetSuggestedRanges(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	if items == nil {
		items = []domain.SuggestedRange{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetEvidenceReferences(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	items, err := h.store.GetEvidenceReferences(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	if items == nil {
		items = []domain.EvidenceReference{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetPlannerDecision(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, FORECAST_ASSIST_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	got, err := h.store.GetPlannerDecision(r.Context(), tenantID, chi.URLParam(r, "jobID"))
	if err != nil {
		h.respondAssistError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) respondAssistError(w http.ResponseWriter, err error) {
	var replay *domain.AssistIdempotentReplayError
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		h.respondJSON(w, http.StatusOK, map[string]string{"resource_id": replay.ResourceID})
		return
	}
	switch {
	case errors.Is(err, domain.ErrForecastAssistJobNotFound), errors.Is(err, domain.ErrForecastModelReleaseNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrForecastJobNotGenerated), errors.Is(err, domain.ErrForecastJobNotInReview),
		errors.Is(err, domain.ErrForecastJobNotOpen), errors.Is(err, domain.ErrNarrativeAlreadySet),
		errors.Is(err, domain.ErrAssistIdempotencyKeyReused):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("forecasting-svc assist request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
