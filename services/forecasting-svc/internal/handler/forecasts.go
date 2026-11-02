package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// FIN-02 forecast handlers.
// ─────────────────────────────────────────────────────────────────────────────

// writeForecast responds with a forecast and its ETag.
func writeForecast(w http.ResponseWriter, status int, forecast *domain.Forecast) {
	w.Header().Set("ETag", etagFor(forecast.Version))
	respondJSON(w, status, forecast)
}

// GenerateForecast handles POST /v1/forecasts/generate.
//
// Validation: JSON body required; domain/scenario/algorithm/granularity enums;
// 2..1000 historical points; horizon 1..120; historical_start_date YYYY-MM-DD.
// legal_entity_id defaults to X-Legal-Entity-Id and must agree with it.
// Writes: forecast_models (status CALCULATED, version 1) + forecast_projections.
// Event: finance.forecast.created (outbox).
// Success: 201 + ETag. Errors: 400 malformed/empty body, 401, 403 legal entity
// or tenant mismatch / authz denied, 409 Idempotency-Key reused, 422 validation.
func (h *Handler) GenerateForecast(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.GenerateForecastRequest
	if !decodeJSON(w, r, rawBody, &req) {
		return
	}

	headerLegalEntity := legalEntityHeader(r)
	if req.LegalEntityID == "" {
		req.LegalEntityID = headerLegalEntity
	} else if headerLegalEntity != "" && req.LegalEntityID != headerLegalEntity {
		writeProblem(w, r, http.StatusForbidden, "legal_entity_mismatch",
			"body legal_entity_id does not match X-Legal-Entity-Id", "legal_entity_id")
		return
	}
	req.ApplyDefaults()
	if !h.validate(w, r, &req) {
		return
	}
	if !h.authorize(w, r, actorID, req.LegalEntityID, ActionForecastPrepare) {
		return
	}

	cmd := buildCommand(r, actorID, "forecast.generate", "", rawBody, 0)
	forecast, err := h.store.CreateForecast(r.Context(), cmd, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		forecast, err = h.store.GetForecast(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/forecasts/"+forecast.ID)
	writeForecast(w, http.StatusCreated, forecast)
}

// ListForecasts handles GET /v1/forecasts.
//
// Query: legal_entity_id, domain, scenario, status (enums validated), limit
// (1..200, default 50), cursor. Newest first; next_cursor when more rows exist.
// Success: 200 {data, count, next_cursor}. Errors: 400 bad filter/limit/cursor,
// 401, 403 authz denied.
func (h *Handler) ListForecasts(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := domain.ForecastListFilter{
		LegalEntityID: query.Get("legal_entity_id"),
		Domain:        domain.ForecastDomain(query.Get("domain")),
		ScenarioType:  domain.ScenarioType(query.Get("scenario")),
		Status:        domain.ForecastStatus(query.Get("status")),
	}
	if filter.Domain != "" && !filter.Domain.Valid() {
		invalidQuery(w, r, "domain must be one of FINANCIAL, PAYROLL, CASH_FLOW, WORKFORCE, TAX_LIABILITY")
		return
	}
	if filter.ScenarioType != "" && !filter.ScenarioType.Valid() {
		invalidQuery(w, r, "scenario must be one of BASELINE, OPTIMISTIC, PESSIMISTIC")
		return
	}
	if filter.Status != "" && !filter.Status.Valid() {
		invalidQuery(w, r, "status must be one of CALCULATED, REVIEWED, APPROVED, PUBLISHED, SUPERSEDED, ARCHIVED")
		return
	}
	page, ok := parsePageRequest(w, r)
	if !ok {
		return
	}
	filter.Page = page

	if !h.authorize(w, r, actorID, authorizationScope(r, filter.LegalEntityID), ActionForecastRead) {
		return
	}
	forecasts, nextCursor, err := h.store.ListForecasts(r.Context(), tenantOf(r), filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: forecasts, Count: len(forecasts), NextCursor: nextCursor})
}

// loadForecastForRequest validates the {id} path parameter, loads the forecast
// in the caller's tenant and authorizes action against its legal entity.
// Errors: 400 bad uuid, 404 not found (including other tenants), 403/503 authz.
func (h *Handler) loadForecastForRequest(w http.ResponseWriter, r *http.Request, actorID, action string) (*domain.Forecast, bool) {
	forecastID := chi.URLParam(r, "id")
	if !requireUUIDPathParam(w, r, "forecast id", forecastID) {
		return nil, false
	}
	forecast, err := h.store.GetForecast(r.Context(), tenantOf(r), forecastID)
	if err != nil {
		h.writeError(w, r, err)
		return nil, false
	}
	if !h.authorize(w, r, actorID, forecast.LegalEntityID, action) {
		return nil, false
	}
	return forecast, true
}

// GetForecast handles GET /v1/forecasts/{id}.
//
// Success: 200 forecast with projections + ETag "v<version>".
// Errors: 400 bad uuid, 401, 403 authz, 404 unknown or other tenant.
func (h *Handler) GetForecast(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	forecast, ok := h.loadForecastForRequest(w, r, actorID, ActionForecastRead)
	if !ok {
		return
	}
	writeForecast(w, http.StatusOK, forecast)
}

// RecalculateForecast handles POST /v1/forecasts/{id}/recalculate.
//
// Requires If-Match. Body: growth_rate_adjustment (-0.99..10) and/or
// scenario_type. Only CALCULATED forecasts; projections are recomputed from the
// stored historical data and the version increments.
// Event: finance.forecast.calculated.
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409 not CALCULATED /
// key reused, 412 stale If-Match, 422 validation, 428 If-Match missing.
func (h *Handler) RecalculateForecast(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	forecast, ok := h.loadForecastForRequest(w, r, actorID, ActionForecastPrepare)
	if !ok {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.RecalculateForecastRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}

	cmd := buildCommand(r, actorID, "forecast.recalculate", forecast.ID, rawBody, expectedVersion)
	updated, err := h.store.RecalculateForecast(r.Context(), cmd, forecast.ID, req)
	h.finishForecastCommand(w, r, cmd, updated, err, http.StatusOK)
}

// forecastCommand is the signature shared by submit/approve/publish/archive.
type forecastCommand func(h *Handler, r *http.Request, cmd store.Command, forecastID string) (*domain.Forecast, error)

// runForecastTransition is the common body of the state-transition handlers:
// actor -> load + authorize -> If-Match -> optional JSON body -> store command.
func (h *Handler) runForecastTransition(w http.ResponseWriter, r *http.Request, action, operation string, command forecastCommand) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	forecast, ok := h.loadForecastForRequest(w, r, actorID, action)
	if !ok {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, false)
	if !ok {
		return
	}
	var ignoredBody map[string]interface{}
	if !decodeJSON(w, r, rawBody, &ignoredBody) {
		return
	}

	cmd := buildCommand(r, actorID, operation, forecast.ID, rawBody, expectedVersion)
	updated, err := command(h, r, cmd, forecast.ID)
	h.finishForecastCommand(w, r, cmd, updated, err, http.StatusOK)
}

// finishForecastCommand resolves an idempotent replay and writes the response.
func (h *Handler) finishForecastCommand(w http.ResponseWriter, r *http.Request, cmd store.Command,
	forecast *domain.Forecast, err error, successStatus int) {
	if replay, isReplay := asReplay(w, err); isReplay {
		forecast, err = h.store.GetForecast(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeForecast(w, successStatus, forecast)
}

// SubmitForecast handles POST /v1/forecasts/{id}/submit.
//
// CALCULATED -> REVIEWED (submitted for approval). Requires If-Match.
// Event: finance.forecast.submitted. Success: 200 + ETag.
// Errors: 400, 401, 403, 404, 409 illegal transition, 412, 428.
func (h *Handler) SubmitForecast(w http.ResponseWriter, r *http.Request) {
	h.runForecastTransition(w, r, ActionForecastPrepare, "forecast.submit",
		func(h *Handler, r *http.Request, cmd store.Command, forecastID string) (*domain.Forecast, error) {
			return h.store.SubmitForecast(r.Context(), cmd, forecastID)
		})
}

// ApproveForecast handles POST /v1/forecasts/{id}/approve.
//
// REVIEWED -> APPROVED. Requires If-Match. Segregation of duties: the approver
// must differ from the creator and the submitter (403 segregation_of_duties).
// After approval the forecast's numbers are immutable.
// Event: finance.forecast.approved. Success: 200 + ETag.
// Errors: 400, 401, 403 SoD/authz, 404, 409 illegal transition, 412, 428.
func (h *Handler) ApproveForecast(w http.ResponseWriter, r *http.Request) {
	h.runForecastTransition(w, r, ActionForecastApprove, "forecast.approve",
		func(h *Handler, r *http.Request, cmd store.Command, forecastID string) (*domain.Forecast, error) {
			return h.store.ApproveForecast(r.Context(), cmd, forecastID)
		})
}

// PublishForecast handles POST /v1/forecasts/{id}/publish.
//
// APPROVED -> PUBLISHED. Requires If-Match. If this forecast is a reforecast
// of a PUBLISHED forecast, that prior forecast becomes SUPERSEDED in the same
// transaction. Events: finance.forecast.published (+ finance.forecast.superseded).
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 409, 412, 428.
func (h *Handler) PublishForecast(w http.ResponseWriter, r *http.Request) {
	h.runForecastTransition(w, r, ActionForecastPublish, "forecast.publish",
		func(h *Handler, r *http.Request, cmd store.Command, forecastID string) (*domain.Forecast, error) {
			return h.store.PublishForecast(r.Context(), cmd, forecastID)
		})
}

// ArchiveForecast handles DELETE /v1/forecasts/{id}.
//
// CALCULATED/REVIEWED -> ARCHIVED (soft delete; rows are never removed).
// Approved/published versions cannot be archived (409). Requires If-Match.
// Event: finance.forecast.archived. Success: 200 archived forecast + ETag.
// Errors: 400, 401, 403, 404, 409, 412, 428.
func (h *Handler) ArchiveForecast(w http.ResponseWriter, r *http.Request) {
	h.runForecastTransition(w, r, ActionForecastPrepare, "forecast.archive",
		func(h *Handler, r *http.Request, cmd store.Command, forecastID string) (*domain.Forecast, error) {
			return h.store.ArchiveForecast(r.Context(), cmd, forecastID)
		})
}

// ReforecastForecast handles POST /v1/forecasts/{id}/reforecast.
//
// Creates a NEW forecast (CALCULATED, prior_forecast_id = {id}) from an
// APPROVED or PUBLISHED source; the source is not modified. Optional body:
// model_name, scenario_type, growth_rate_adjustment, historical_data.
// If-Match is optional (the source is not mutated). Event: finance.forecast.created.
// Success: 201 new forecast + ETag. Errors: 400, 401, 403, 404, 409 source not
// APPROVED/PUBLISHED, 422 validation.
func (h *Handler) ReforecastForecast(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	source, ok := h.loadForecastForRequest(w, r, actorID, ActionForecastPrepare)
	if !ok {
		return
	}
	expectedVersion, ok := optionalIfMatch(w, r)
	if !ok {
		return
	}
	if expectedVersion != 0 && expectedVersion != source.Version {
		h.writeError(w, r, domain.NewStaleVersionError(expectedVersion, source.Version))
		return
	}
	rawBody, ok := readBody(w, r, false)
	if !ok {
		return
	}
	var req domain.ReforecastRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}

	cmd := buildCommand(r, actorID, "forecast.reforecast", source.ID, rawBody, 0)
	created, err := h.store.Reforecast(r.Context(), cmd, source.ID, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		created, err = h.store.GetForecast(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/forecasts/"+created.ID)
	writeForecast(w, http.StatusCreated, created)
}
