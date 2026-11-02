package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/handler"
	"zoiko.io/forecasting-svc/internal/store"
	"zoiko.io/forecasting-svc/internal/testdb"
)

// Ported intent of the old TestGenerateAndLifecycleForecast, end to end
// against real Postgres as the NOSUPERUSER NOBYPASSRLS app role.
func TestHTTP_ForecastAndAssistFlow_Postgres(t *testing.T) {
	_, app := testdb.Open(t, "forecast_handler_it")
	router := handler.NewRouter(handler.NewHandler(store.NewPgStore(app), allowAll(), zap.NewNop()), app)

	if w := send(router, http.MethodGet, "/readyz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("readyz: %d", w.Code)
	}

	key := func(k string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Idempotency-Key", k) }
	}
	as := func(principal, ifMatch string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("X-Principal-Id", principal)
			if ifMatch != "" {
				r.Header.Set("If-Match", ifMatch)
			}
		}
	}

	w := send(router, http.MethodPost, "/v1/forecasts/generate", validForecast, key("gen-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("generate: %d %s", w.Code, w.Body.String())
	}
	var fc domain.Forecast
	_ = json.Unmarshal(w.Body.Bytes(), &fc)
	if len(fc.Projections) != 6 || fc.Projections[0].ProjectedAmount <= 0 {
		t.Fatalf("projections: %+v", fc.Projections)
	}
	replay := send(router, http.MethodPost, "/v1/forecasts/generate", validForecast, key("gen-1"))
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", replay.Code, replay.Header())
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/generate", `{"model_name":"other"}`, key("gen-1")); w.Code != http.StatusConflict && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with other body: %d", w.Code)
	}

	if w := send(router, http.MethodGet, "/v1/forecasts?legal_entity_id=entity-test", "", nil); w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/"+fc.ID+"/recalculate", `{"growth_rate_adjustment":0.1}`, as("principal-test", `"v1"`)); w.Code != http.StatusOK {
		t.Fatalf("recalculate: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/"+fc.ID+"/submit", `{}`, as("principal-test", `"v1"`)); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale submit: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/"+fc.ID+"/submit", `{}`, as("principal-test", `"v2"`)); w.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/"+fc.ID+"/approve", `{}`, as("principal-test", `"v3"`)); w.Code != http.StatusForbidden {
		t.Fatalf("maker approves own forecast: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecasts/"+fc.ID+"/approve", `{}`, as("checker", `"v3"`)); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodDelete, "/v1/forecasts/"+fc.ID, "", as("principal-test", `"v4"`)); w.Code != http.StatusConflict {
		t.Fatalf("archive approved: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodGet, "/v1/forecasts/"+fc.ID, "", func(r *http.Request) { r.Header.Set("X-Tenant-Id", "other-tenant") }); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: %d", w.Code)
	}

	// AI-05: register, evaluate (second principal), suggest, review, accept.
	w = send(router, http.MethodPost, "/v1/forecast-assist/model-releases", `{"domain_name":"cash","model_provider":"p","model_version":"v1"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("register release: %d %s", w.Code, w.Body.String())
	}
	var rel domain.ForecastModelRelease
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	if w := send(router, http.MethodPost, "/v1/forecast-assist/model-releases/"+rel.ReleaseID+":approve", `{"evaluation_reference":"e-1"}`, as("evaluator", `"v1"`)); w.Code != http.StatusOK {
		t.Fatalf("approve release: %d %s", w.Code, w.Body.String())
	}
	w = send(router, http.MethodPost, "/v1/forecast-assist/suggest-drivers",
		`{"domain_name":"cash","model_provider":"p","model_version":"v1","plan_version":"pv","planning_purpose":"pp",
		  "drivers":[{"driver_name":"growth","suggested_value":0.05,"rationale":"trend"}],"evidence":[{"source_ref":"src-1"}]}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("suggest drivers: %d %s", w.Code, w.Body.String())
	}
	var job domain.ForecastAssistJob
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if w := send(router, http.MethodPost, "/v1/forecast-assist/jobs/"+job.JobID+":start-review", `{}`, as("planner", `"v1"`)); w.Code != http.StatusOK {
		t.Fatalf("start review: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecast-assist/jobs/"+job.JobID+":accept", `{}`, as("principal-test", `"v2"`)); w.Code != http.StatusForbidden {
		t.Fatalf("requester accepts own suggestion: %d %s", w.Code, w.Body.String())
	}
	if w := send(router, http.MethodPost, "/v1/forecast-assist/jobs/"+job.JobID+":accept", `{"target_forecast_id":"`+fc.ID+`"}`, as("planner", `"v2"`)); w.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	for _, sub := range []string{"drivers", "ranges", "evidence", "decision"} {
		if w := send(router, http.MethodGet, "/v1/forecast-assist/jobs/"+job.JobID+"/"+sub, "", nil); w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", sub, w.Code)
		}
	}
	if w := send(router, http.MethodGet, "/v1/forecast-assist/jobs/"+job.JobID, "", func(r *http.Request) { r.Header.Set("X-Tenant-Id", "other-tenant") }); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant job: %d", w.Code)
	}
}
