package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/events"
	"zoiko.io/reconciliation-intelligence-svc/internal/handler"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

func setupTestRouter() http.Handler {
	logger := zap.NewNop()
	memStore := store.NewMemoryStore()
	publisher := events.NewPublisher([]string{"localhost:9092"}, "zoiko.reconciliation-intelligence.events", logger)
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

	if resp["status"] != "ok" || resp["service"] != "reconciliation-intelligence-svc" {
		t.Fatalf("unexpected response body: %v", resp)
	}
}

func TestAnalyzeAndLifecycleReconciliation(t *testing.T) {
	router := setupTestRouter()

	// 1. Analyze Reconciliation
	analyzeReq := domain.AnalyzeReconciliationRequest{
		LegalEntityID: "LE-3003",
		JobName:       "Bank Reconciliation Match Q3",
		SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{
			{RefID: "TX-101", Amount: 500.00, Date: "2026-07-01"},
			{RefID: "TX-102", Amount: 1200.50, Date: "2026-07-02"}, // Mismatch
			{RefID: "TX-103", Amount: 75.00, Date: "2026-07-03"},   // Missing in B
		},
		TransactionsB: []domain.TransactionItem{
			{RefID: "TX-101", Amount: 500.00, Date: "2026-07-01"},
			{RefID: "TX-102", Amount: 1220.50, Date: "2026-07-02"}, // Difference = 20.00
		},
	}

	body, _ := json.Marshal(analyzeReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-rec-88")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var job domain.ReconciliationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("failed to unmarshal created job: %v", err)
	}

	if job.ID == "" {
		t.Fatalf("expected non-empty job ID")
	}

	if job.MatchedCount != 1 || job.UnmatchedCount != 2 {
		t.Fatalf("expected 1 matched and 2 unmatched, got %d matched and %d unmatched", job.MatchedCount, job.UnmatchedCount)
	}

	// 2. Get Job by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/reconciliations/"+job.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	getRec := httptest.NewRecorder()

	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", getRec.Code)
	}

	// 3. Apply Resolution
	itemId := job.UnmatchedItems[0].ID
	applyBody, _ := json.Marshal(domain.ApplyResolutionRequest{
		ResolutionStatus: domain.StatusApproved,
		ResolutionNotes:  "Approved write-off of $20.00 timing difference",
	})

	applyReq := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/"+job.ID+"/resolutions/"+itemId+"/apply", bytes.NewBuffer(applyBody))
	applyReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	applyReq.Header.Set("Content-Type", "application/json")
	applyRec := httptest.NewRecorder()

	router.ServeHTTP(applyRec, applyReq)

	if applyRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on resolution apply, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	// 4. List Jobs
	listReq := httptest.NewRequest(http.MethodGet, "/v1/reconciliations?legal_entity_id=LE-3003", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	listRec := httptest.NewRecorder()

	router.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", listRec.Code)
	}

	// 5. Archive Job
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/reconciliations/"+job.ID, nil)
	delReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	delRec := httptest.NewRecorder()

	router.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on archive, got %d", delRec.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	router := setupTestRouter()

	invalidReq := domain.AnalyzeReconciliationRequest{
		LegalEntityID: "", // Missing
	}

	body, _ := json.Marshal(invalidReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-rec-88")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rec.Code)
	}
}
