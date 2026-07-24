package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/handler"
	"zoiko.io/forecasting-svc/internal/store"
)

func setupTestRouter() http.Handler {
	logger := zap.NewNop()
	memStore := store.NewMemoryStore()
	publisher := events.NewPublisher([]string{"localhost:9092"}, "zoiko.forecasting.events", logger)
	authzClient := authz.NewClient("http://localhost:8089", logger)
	h := handler.NewHandler(memStore, publisher, authzClient, logger)

	return handler.NewRouter(h)
}

func TestHealthCheck(t *testing.T) {
	router := setupTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal health response: %v", err)
	}

	if resp["status"] != "ok" || resp["service"] != "forecasting-svc" {
		t.Fatalf("unexpected response body: %v", resp)
	}
}

func TestGenerateAndLifecycleForecast(t *testing.T) {
	router := setupTestRouter()

	// 1. Generate Forecast
	genReq := domain.GenerateForecastRequest{
		LegalEntityID:       "LE-1001",
		ModelName:           "2026 Q3 Financial Cash Flow Forecast",
		Domain:              domain.DomainCashFlow,
		ScenarioType:        domain.ScenarioOptimistic,
		AlgorithmType:       domain.AlgorithmLinearTrend,
		Granularity:         domain.GranularityMonthly,
		HorizonPeriods:      6,
		HistoricalData:      []float64{10000, 12000, 11500, 13000, 14500, 16000},
		HistoricalStartDate: "2025-01-01",
	}

	body, _ := json.Marshal(genReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/forecasts/generate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-test-123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var createdModel domain.ForecastModel
	if err := json.Unmarshal(rec.Body.Bytes(), &createdModel); err != nil {
		t.Fatalf("failed to unmarshal created forecast: %v", err)
	}

	if createdModel.ID == "" {
		t.Fatalf("expected non-empty model ID")
	}

	if len(createdModel.Projections) != 6 {
		t.Fatalf("expected 6 projections, got %d", len(createdModel.Projections))
	}

	// Verify Projections math (Optimistic multiplier applied + trend)
	if createdModel.Projections[0].ProjectedAmount <= 0 {
		t.Fatalf("expected positive projection amount, got %f", createdModel.Projections[0].ProjectedAmount)
	}

	// 2. Get Forecast by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/forecasts/"+createdModel.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-test-123")
	getRec := httptest.NewRecorder()

	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", getRec.Code)
	}

	// 3. List Forecasts
	listReq := httptest.NewRequest(http.MethodGet, "/v1/forecasts?legal_entity_id=LE-1001", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-test-123")
	listRec := httptest.NewRecorder()

	router.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", listRec.Code)
	}

	var listResp map[string]interface{}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listResp)
	if listResp["count"].(float64) < 1 {
		t.Fatalf("expected at least 1 model in list")
	}

	// 4. Recalculate Forecast
	recalcReqBody, _ := json.Marshal(domain.RecalculateRequest{
		GrowthRateAdjustment: 0.10, // +10%
		ScenarioType:         domain.ScenarioOptimistic,
	})
	recalcReq := httptest.NewRequest(http.MethodPost, "/v1/forecasts/"+createdModel.ID+"/recalculate", bytes.NewBuffer(recalcReqBody))
	recalcReq.Header.Set("X-Tenant-ID", "tenant-test-123")
	recalcReq.Header.Set("Content-Type", "application/json")
	recalcRec := httptest.NewRecorder()

	router.ServeHTTP(recalcRec, recalcReq)

	if recalcRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on recalculate, got %d", recalcRec.Code)
	}

	// 5. Archive Forecast
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/forecasts/"+createdModel.ID, nil)
	delReq.Header.Set("X-Tenant-ID", "tenant-test-123")
	delRec := httptest.NewRecorder()

	router.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on archive, got %d", delRec.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	router := setupTestRouter()

	invalidReq := domain.GenerateForecastRequest{
		LegalEntityID: "", // Missing
		ModelName:     "Test",
	}

	body, _ := json.Marshal(invalidReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/forecasts/generate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-test-123")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rec.Code)
	}
}
