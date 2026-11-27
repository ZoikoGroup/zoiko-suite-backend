package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

func analyzedJob(t *testing.T, router http.Handler) domain.ReconciliationJob {
	t.Helper()
	rec := postAnalyze(router, "LE-3003", runLE3003)
	if rec.Code != http.StatusCreated {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body.String())
	}
	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	return job
}

func apply(router http.Handler, jobID, itemID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/"+jobID+"/resolutions/"+itemID+"/apply", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A malformed item id used to reach Postgres and come back as a 404 carrying
// the raw "invalid input syntax for type uuid ... SQLSTATE 22P02" error.
func TestApply_MalformedItemID_400_NoDatabaseErrorLeaked(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router)
	rec := apply(router, job.ID, "nope", `{"suggestion_status":"REJECTED","resolution_notes":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SQLSTATE") {
		t.Fatalf("database error leaked: %s", rec.Body.String())
	}
}

// An ARCHIVED job's suggestions are the audit trail: a decision on one is 409.
func TestApply_OnArchivedJob_409(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router)
	req := httptest.NewRequest(http.MethodDelete, "/v1/reconciliations/"+job.ID, nil)
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	rec = apply(router, job.ID, job.UnmatchedItems[0].ID, `{"suggestion_status":"REJECTED","resolution_notes":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decision on an archived job must be 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyze_OversizedBody_413(t *testing.T) {
	router := setupTestRouter(t)
	body := `{"legal_entity_id":"LE-3003","job_name":"` + strings.Repeat("x", 70*1024) + `","bank_reconciliation_run_id":"` + runLE3003 + `"}`
	if rec := postAnalyzeRaw(router, []byte(body)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}
