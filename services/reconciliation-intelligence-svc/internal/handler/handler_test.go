package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

func TestHealthCheck(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	withEnvelope(req)
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
	router := setupTestRouter(t)

	// 1. Analyze the UNMATCHED lines of a bank-reconciliation run.
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(analyzeBody("LE-3003", runLE3003)))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-rec-88")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
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
	if job.BankReconciliationRunID != runLE3003 {
		t.Fatalf("job not traceable to its run: bank_reconciliation_run_id = %q", job.BankReconciliationRunID)
	}
	if job.TotalProcessedCount != 2 || job.UnmatchedCount != 2 || len(job.UnmatchedItems) != 2 {
		t.Fatalf("expected both UNMATCHED lines processed and suggested on, got processed %d, unmatched %d, items %d",
			job.TotalProcessedCount, job.UnmatchedCount, len(job.UnmatchedItems))
	}
	if job.UnmatchedItems[0].StatementLineID != "sl-3003-a" || job.UnmatchedItems[1].StatementLineID != "sl-3003-b" {
		t.Fatalf("suggestions not traceable to their statement lines: %q, %q",
			job.UnmatchedItems[0].StatementLineID, job.UnmatchedItems[1].StatementLineID)
	}

	// 2. Get Job by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/reconciliations/"+job.ID, nil)
	withEnvelope(getReq)
	getReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	getRec := httptest.NewRecorder()

	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", getRec.Code, getRec.Body.String())
	}

	// 3. Apply Resolution
	itemId := job.UnmatchedItems[0].ID
	applyBody, _ := json.Marshal(domain.ApplyResolutionRequest{
		ResolutionStatus: domain.StatusApproved,
		ResolutionNotes:  "Reviewed; matched manually in bank reconciliation",
	})

	applyReq := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/"+job.ID+"/resolutions/"+itemId+"/apply", bytes.NewBuffer(applyBody))
	withEnvelope(applyReq)
	applyReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	applyReq.Header.Set("Content-Type", "application/json")
	applyReq.Header.Set("X-Principal-Id", "principal-01")
	applyRec := httptest.NewRecorder()

	router.ServeHTTP(applyRec, applyReq)

	if applyRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on resolution apply, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	// 4. List Jobs
	listReq := httptest.NewRequest(http.MethodGet, "/v1/reconciliations?legal_entity_id=LE-3003", nil)
	withEnvelope(listReq)
	listReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	listRec := httptest.NewRecorder()

	router.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", listRec.Code, listRec.Body.String())
	}

	// 5. Archive Job
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/reconciliations/"+job.ID, nil)
	withEnvelope(delReq)
	delReq.Header.Set("X-Tenant-ID", "tenant-rec-88")
	delReq.Header.Set("X-Principal-Id", "principal-01")
	delRec := httptest.NewRecorder()

	router.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on archive, got %d", delRec.Code)
	}
}

// A8: every suggestion in the analyze response names the rule that fired and
// the exact tolerance-policy version used, so the confidence number has a
// machine-readable basis.
func TestAnalyze_ResponseCarriesScoreProvenance(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(analyzeBody("LE-3003", runLE3003)))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-rec-a8")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		UnmatchedItems []map[string]any `json:"unmatched_items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.UnmatchedItems) != 2 {
		t.Fatalf("expected 2 suggestions, got %d", len(body.UnmatchedItems))
	}
	// runLE3003 has a 75.00 line (at/above the 10 tolerance) and a 5.00
	// line (below it); the fake policy registry answers version 1.
	wantRules := []string{"MISSING_REFERENCE_MANUAL_REVIEW", "MISSING_REFERENCE_WRITE_OFF"}
	for i, item := range body.UnmatchedItems {
		if item["heuristic_rule"] != wantRules[i] {
			t.Errorf("item %d heuristic_rule = %v, want %s", i, item["heuristic_rule"], wantRules[i])
		}
		if item["tolerance_version"] != float64(1) {
			t.Errorf("item %d tolerance_version = %v, want 1", i, item["tolerance_version"])
		}
	}
}

func TestValidationErrors(t *testing.T) {
	router := setupTestRouter(t)

	invalidReq := domain.AnalyzeReconciliationRequest{
		LegalEntityID: "", // Missing
	}

	body, _ := json.Marshal(invalidReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-rec-88")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rec.Code)
	}
}
