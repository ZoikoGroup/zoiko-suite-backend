// Package handler is the HTTP layer of forecasting-svc.
//
// Request path: router (this file) -> envelope middleware (§4 input contract)
// -> TenantMiddleware -> handler (forecasts.go, model_releases.go,
// assist_jobs.go) -> store (internal/store) -> Postgres
// (deployments/migrations). See ROUTES.md for the full table.
package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/forecasting-svc/internal/envelope"
	"zoiko.io/forecasting-svc/internal/health"
	customMiddleware "zoiko.io/forecasting-svc/internal/middleware"
	"zoiko.io/forecasting-svc/internal/store"
)

// Authorization actions sent to authorization-svc. Names follow the FIN-02
// permission list (forecast.prepare / review / approve / publish) and the AI-05
// separation of model publication from evaluation approval.
const (
	ActionForecastRead    = "FORECAST_READ"
	ActionForecastPrepare = "FORECAST_PREPARE" // generate, recalculate, submit, archive, reforecast
	ActionForecastApprove = "FORECAST_APPROVE"
	ActionForecastPublish = "FORECAST_PUBLISH"

	ActionModelReleaseRegister = "FORECAST_MODEL_RELEASE_REGISTER"
	ActionModelReleaseApprove  = "FORECAST_MODEL_RELEASE_APPROVE"

	ActionAssistRead    = "FORECAST_ASSIST_READ"
	ActionAssistSuggest = "FORECAST_ASSIST_SUGGEST" // suggest-drivers, suggest-range, generate-narrative
	ActionAssistDecide  = "FORECAST_ASSIST_DECIDE"  // start-review, accept, reject
)

// Authorizer is the part of authz.Client the handlers use. It must fail
// closed: any error is a refusal.
type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler holds the dependencies of every HTTP handler.
type Handler struct {
	store  store.Store
	authz  Authorizer
	logger *zap.Logger
}

func NewHandler(s store.Store, a Authorizer, l *zap.Logger) *Handler {
	return &Handler{store: s, authz: a, logger: l}
}

// authorize asks authorization-svc and writes 403/503 on refusal.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, actorID, legalEntityID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), actorID, legalEntityID, action); err != nil {
		h.writeError(w, r, err)
		return false
	}
	return true
}

// NewRouter wires every route. readiness may be nil (readyz then reports 503).
func NewRouter(h *Handler, readiness health.Pinger) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Tenant gate, then the §4 envelope (platform order). Both exempt the
	// probes, so /healthz and /readyz answer without identity headers, while
	// every other path — including unknown ones — needs a verified tenant.
	r.Use(customMiddleware.TenantMiddleware)
	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4): every write
	// must carry tenant, principal, request id, correlation id, source channel,
	// legal entity and Idempotency-Key.
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.LivenessHandler())
	r.Get("/readyz", health.ReadinessHandler(readiness))

	// FIN-02 forecasts.
	r.Post("/v1/forecasts/generate", h.GenerateForecast)
	r.Get("/v1/forecasts", h.ListForecasts)
	r.Get("/v1/forecasts/{id}", h.GetForecast)
	r.Post("/v1/forecasts/{id}/recalculate", h.RecalculateForecast)
	r.Post("/v1/forecasts/{id}/submit", h.SubmitForecast)
	r.Post("/v1/forecasts/{id}/approve", h.ApproveForecast)
	r.Post("/v1/forecasts/{id}/publish", h.PublishForecast)
	r.Post("/v1/forecasts/{id}/reforecast", h.ReforecastForecast)
	r.Delete("/v1/forecasts/{id}", h.ArchiveForecast)

	// AI-05 model releases.
	r.Post("/v1/forecast-assist/model-releases", h.RegisterModelRelease)
	r.Get("/v1/forecast-assist/model-releases", h.ListModelReleases)
	r.Get("/v1/forecast-assist/model-releases/{releaseID}", h.GetModelRelease)
	r.Post("/v1/forecast-assist/model-releases/{releaseID}:approve", h.ApproveModelRelease)

	// AI-05 suggestion commands.
	r.Post("/v1/forecast-assist/suggest-drivers", h.SuggestDrivers)
	r.Post("/v1/forecast-assist/suggest-range", h.SuggestRange)
	r.Post("/v1/forecast-assist/generate-narrative", h.GenerateNarrative)

	// AI-05 jobs: planner review and reads.
	r.Get("/v1/forecast-assist/jobs", h.ListAssistJobs)
	r.Get("/v1/forecast-assist/jobs/{jobID}", h.GetAssistJob)
	r.Post("/v1/forecast-assist/jobs/{jobID}:start-review", h.StartPlannerReview)
	r.Post("/v1/forecast-assist/jobs/{jobID}:accept", h.AcceptSuggestion)
	r.Post("/v1/forecast-assist/jobs/{jobID}:reject", h.RejectSuggestion)
	r.Get("/v1/forecast-assist/jobs/{jobID}/drivers", h.GetSuggestedDrivers)
	r.Get("/v1/forecast-assist/jobs/{jobID}/ranges", h.GetSuggestedRanges)
	r.Get("/v1/forecast-assist/jobs/{jobID}/evidence", h.GetEvidenceReferences)
	r.Get("/v1/forecast-assist/jobs/{jobID}/decision", h.GetPlannerDecision)

	// Unknown routes and wrong methods also answer in problem+json.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "route_not_found", "no route matches "+r.Method+" "+r.URL.Path, "")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"type":"` + problemTypeBase + `method-not-allowed","title":"Method not allowed.","status":405,"code":"method_not_allowed","retryable":false}`))
	})

	return r
}
