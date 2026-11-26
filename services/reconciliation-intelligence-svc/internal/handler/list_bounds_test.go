package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// job_name is VARCHAR(128); an over-long name used to fail the INSERT and
// answer 500. It is now the caller's 400, and nothing is stored.
func TestAnalyze_JobNameTooLong_400(t *testing.T) {
	router := setupTestRouter(t)
	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "LE-3003", "job_name": strings.Repeat("j", 129), "bank_reconciliation_run_id": runLE3003,
	})
	if rec := postAnalyzeRaw(router, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]string{
		"legal_entity_id": "LE-3003", "job_name": strings.Repeat("j", 128), "bank_reconciliation_run_id": runLE3003,
	})
	if rec := postAnalyzeRaw(router, body); rec.Code != http.StatusCreated {
		t.Fatalf("a 128-character name is allowed; got %d: %s", rec.Code, rec.Body.String())
	}
}

type jobPage struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	Count  int `json:"count"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func listPage(t *testing.T, router http.Handler, query string) (int, jobPage) {
	t.Helper()
	rec := get(router, "/v1/reconciliations?legal_entity_id=LE-3003"+query, "principal-test")
	var p jobPage
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

// The list is always bounded and paged; it used to return every job.
func TestListJobs_PagedAndBounded(t *testing.T) {
	router := setupTestRouter(t)
	for i := 0; i < 3; i++ {
		createJob(t, router)
	}

	code, all := listPage(t, router, "")
	if code != http.StatusOK || all.Count != 3 || all.Limit != 50 || all.Offset != 0 {
		t.Fatalf("default page = %d %+v, want 3 jobs with limit 50 offset 0", code, all)
	}
	_, first := listPage(t, router, "&limit=1")
	_, second := listPage(t, router, "&limit=1&offset=1")
	if first.Count != 1 || second.Count != 1 || first.Data[0].ID == second.Data[0].ID {
		t.Fatalf("pages must hold one distinct job each: %+v / %+v", first, second)
	}
	if _, past := listPage(t, router, "&offset=10"); past.Count != 0 {
		t.Fatalf("offset past the end must be an empty page, got %+v", past)
	}
}

func TestListJobs_InvalidFiltersAndPaging_400(t *testing.T) {
	router := setupTestRouter(t)
	for _, q := range []string{"&limit=0", "&limit=201", "&limit=x", "&offset=-1", "&status=BOGUS", "&source_system_a=NOPE"} {
		if code, _ := listPage(t, router, q); code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", q, code)
		}
	}
}
