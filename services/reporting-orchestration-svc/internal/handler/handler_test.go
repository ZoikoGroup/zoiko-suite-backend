package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/events"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

func newRouter() http.Handler {
	logger := zap.NewNop()
	s := store.NewMemoryStore()
	p := events.NewPublisher([]string{"localhost:9092"}, "zoiko.reporting-orchestration.events", logger)
	a := authz.NewClient("http://localhost:8089", logger)
	h := handler.NewHandler(s, p, a, logger)
	return handler.NewRouter(h)
}

func TestHealthCheck(t *testing.T) {
	router := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["status"] != "ok" || resp["service"] != "reporting-orchestration-svc" {
		t.Fatalf("unexpected body: %v", resp)
	}
}

func TestCreateDefinitionAndTriggerRun(t *testing.T) {
	router := newRouter()

	// 1. Create Report Definition
	createReq := domain.CreateDefinitionRequest{
		LegalEntityID: "LE-4004",
		ReportName:    "Q3 Financial Summary Report",
		ReportType:    domain.ReportTypeFinancialSummary,
		OutputFormat:  domain.FormatJSON,
		DataSources:   []string{"ledger-svc", "treasury-svc"},
		IsScheduled:   false,
	}
	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var def domain.ReportDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &def); err != nil {
		t.Fatalf("unmarshal definition error: %v", err)
	}
	if def.ID == "" {
		t.Fatal("expected non-empty definition ID")
	}
	if def.Status != domain.DefStatusActive {
		t.Fatalf("expected status ACTIVE, got %s", def.Status)
	}

	// 2. Get Definition by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/reports/definitions/"+def.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", getRec.Code)
	}

	// 3. List Definitions
	listReq := httptest.NewRequest(http.MethodGet, "/v1/reports/definitions?legal_entity_id=LE-4004", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", listRec.Code)
	}
	var listResp map[string]interface{}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listResp)
	if int(listResp["count"].(float64)) < 1 {
		t.Fatal("expected at least 1 definition in list")
	}

	// 4. Trigger Report Run
	runBody, _ := json.Marshal(domain.TriggerRunRequest{
		TriggeredBy: domain.TriggerManual,
		PeriodStart: "2026-07-01",
		PeriodEnd:   "2026-09-30",
	})
	runReq := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions/"+def.ID+"/runs", bytes.NewBuffer(runBody))
	runReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	runReq.Header.Set("Content-Type", "application/json")
	runRec := httptest.NewRecorder()
	router.ServeHTTP(runRec, runReq)

	if runRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on run, got %d: %s", runRec.Code, runRec.Body.String())
	}

	var run domain.ReportRun
	if err := json.Unmarshal(runRec.Body.Bytes(), &run); err != nil {
		t.Fatalf("unmarshal run error: %v", err)
	}
	if run.ID == "" {
		t.Fatal("expected non-empty run ID")
	}
	if run.Status != domain.RunStatusCompleted {
		t.Fatalf("expected run status COMPLETED, got %s", run.Status)
	}
	if run.RowCount == 0 {
		t.Fatal("expected non-zero row_count after orchestration")
	}
	if run.OutputLocation == "" {
		t.Fatal("expected non-empty output_location")
	}

	// 5. Get Run by ID
	getRunReq := httptest.NewRequest(http.MethodGet, "/v1/reports/runs/"+run.ID, nil)
	getRunReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	getRunRec := httptest.NewRecorder()
	router.ServeHTTP(getRunRec, getRunReq)
	if getRunRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get run, got %d", getRunRec.Code)
	}

	// 6. List Runs
	listRunsReq := httptest.NewRequest(http.MethodGet, "/v1/reports/runs?definition_id="+def.ID, nil)
	listRunsReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	listRunsRec := httptest.NewRecorder()
	router.ServeHTTP(listRunsRec, listRunsReq)
	if listRunsRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on list runs, got %d", listRunsRec.Code)
	}

	// 7. Update Definition Status (pause)
	patchBody, _ := json.Marshal(map[string]string{"status": "PAUSED"})
	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/reports/definitions/"+def.ID+"/status", bytes.NewBuffer(patchBody))
	patchReq.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	patchReq.Header.Set("Content-Type", "application/json")
	patchRec := httptest.NewRecorder()
	router.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on status patch, got %d: %s", patchRec.Code, patchRec.Body.String())
	}
}

func TestValidationErrors(t *testing.T) {
	router := newRouter()

	// Missing legal_entity_id
	body, _ := json.Marshal(domain.CreateDefinitionRequest{ReportName: "Test"})
	req := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
	}
}

func TestTriggerRunOnMissingDefinition(t *testing.T) {
	router := newRouter()

	runBody, _ := json.Marshal(domain.TriggerRunRequest{TriggeredBy: domain.TriggerManual})
	req := httptest.NewRequest(http.MethodPost, "/v1/reports/definitions/non-existent-id/runs", bytes.NewBuffer(runBody))
	req.Header.Set("X-Tenant-ID", "tenant-rpt-77")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", rec.Code)
	}
}
