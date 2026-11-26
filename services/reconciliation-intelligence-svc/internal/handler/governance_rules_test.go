package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// The analysis requester in these tests is withEnvelope's "principal-test";
// "reviewer-01" is an independent reviewer.
const reviewer = "reviewer-01"

func decide(router http.Handler, jobID, itemID, principal, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/"+jobID+"/resolutions/"+itemID+"/apply", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principal)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func archive(router http.Handler, jobID string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/v1/reconciliations/"+jobID, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// problemCode asserts an RFC 9457 body with the given status and code.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q, want application/problem+json", ct)
	}
	var p struct {
		Type, Title, Code string
		Status            int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Type == "" || p.Title == "" || p.Status != status || p.Code != code {
		t.Fatalf("problem body %s, want status %d code %q", rec.Body.String(), status, code)
	}
}

func jobVersion(t *testing.T, router http.Handler, jobID string) int {
	t.Helper()
	rec := get(router, "/v1/reconciliations/"+jobID, "principal-test")
	var j domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	return j.Version
}

// Global Authorization / SoD standard: requestor self-approval is denied.
func TestSoD_RequesterCannotApproveOrExecute_MayReject(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router) // requested by principal-test
	if job.RequestedBy != "principal-test" || job.Version != 1 {
		t.Fatalf("job must record its requester and start at version 1: %+v", job)
	}
	a, b := job.UnmatchedItems[0].ID, job.UnmatchedItems[1].ID

	problemCode(t, decide(router, job.ID, a, "principal-test", `{"suggestion_status":"APPROVED","resolution_notes":"mine"}`, nil),
		http.StatusForbidden, "segregation_of_duties")
	if rec := decide(router, job.ID, b, "principal-test", `{"suggestion_status":"REJECTED","resolution_notes":"known reversal"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("requester may reject: %d %s", rec.Code, rec.Body.String())
	}
	rec := decide(router, job.ID, a, reviewer, `{"suggestion_status":"APPROVED","resolution_notes":"agreed"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("independent reviewer approves: %d %s", rec.Code, rec.Body.String())
	}
	var s domain.ReconciliationSuggestion
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.DecidedBy != reviewer || s.DecidedAt == nil || s.ResolutionStatus != domain.StatusApproved {
		t.Fatalf("decision must record the decider and keep the legacy alias: %+v", s)
	}
}

// Global API Contract Standard §10: If-Match carries the aggregate version.
func TestIfMatch_StaleMalformedCurrent_AndVersionAdvances(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router)
	a, b := job.UnmatchedItems[0].ID, job.UnmatchedItems[1].ID
	body := `{"suggestion_status":"REJECTED","resolution_notes":"no"}`

	problemCode(t, decide(router, job.ID, a, reviewer, body, map[string]string{"If-Match": `"7"`}), http.StatusPreconditionFailed, "version_mismatch")
	problemCode(t, decide(router, job.ID, a, reviewer, body, map[string]string{"If-Match": "yesterday"}), http.StatusBadRequest, "invalid_if_match")
	if rec := decide(router, job.ID, a, reviewer, body, map[string]string{"If-Match": `"1"`}); rec.Code != http.StatusOK {
		t.Fatalf("current If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if v := jobVersion(t, router, job.ID); v != 2 {
		t.Fatalf("version after one decision = %d, want 2", v)
	}
	problemCode(t, decide(router, job.ID, b, reviewer, body, map[string]string{"If-Match": `"1"`}), http.StatusPreconditionFailed, "version_mismatch")
}

func TestArchive_ReturnsJob_ThenReadOnly(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router)

	problemCode(t, archive(router, job.ID, map[string]string{"If-Match": `"9"`}), http.StatusPreconditionFailed, "version_mismatch")
	rec := archive(router, job.ID, map[string]string{"If-Match": `"1"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	var j domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	if j.Status != "ARCHIVED" || j.Version != 2 || j.ArchivedBy != "principal-test" || j.ArchivedAt == nil {
		t.Fatalf("archived job: %+v", j)
	}
	problemCode(t, archive(router, job.ID, nil), http.StatusConflict, "job_archived")
	problemCode(t, decide(router, job.ID, job.UnmatchedItems[0].ID, reviewer, `{"suggestion_status":"REJECTED","resolution_notes":"x"}`, nil),
		http.StatusConflict, "job_archived")
}

func TestApply_BodyRules(t *testing.T) {
	router := setupTestRouter(t)
	job := analyzedJob(t, router)
	item := job.UnmatchedItems[0].ID
	for name, body := range map[string]string{
		"missing suggestion_status (never read as approval)": `{"resolution_notes":"x"}`,
		"missing resolution_notes (INV-18 reason required)":  `{"suggestion_status":"APPROVED"}`,
		"unknown field":                  `{"suggestion_status":"APPROVED","resolution_notes":"x","amount":"5.00"}`,
		"conflicting status and alias":   `{"suggestion_status":"APPROVED","resolution_status":"REJECTED","resolution_notes":"x"}`,
		"a human cannot set RECOMMENDED": `{"suggestion_status":"RECOMMENDED","resolution_notes":"x"}`,
	} {
		rec := decide(router, job.ID, item, reviewer, body, nil)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %d %s, want 400 problem+json", name, rec.Code, rec.Body.String())
		}
	}
	problemCode(t, decide(router, "nope", item, reviewer, `{"suggestion_status":"REJECTED","resolution_notes":"x"}`, nil), http.StatusBadRequest, "invalid_id")
	problemCode(t, get(router, "/v1/reconciliations/not-a-uuid", "principal-test"), http.StatusBadRequest, "invalid_id")
}

func TestAnalyze_BodyTenantMustRepeatHeader(t *testing.T) {
	router := setupTestRouter(t)
	mk := func(tenant string) []byte {
		b, _ := json.Marshal(map[string]string{"tenant_id": tenant, "legal_entity_id": "LE-3003", "job_name": "j", "bank_reconciliation_run_id": runLE3003})
		return b
	}
	if rec := postAnalyzeRaw(router, mk("tenant-test")); rec.Code != http.StatusCreated {
		t.Fatalf("matching body tenant: %d %s", rec.Code, rec.Body.String())
	}
	problemCode(t, postAnalyzeRaw(router, mk("someone-else")), http.StatusForbidden, "tenant_mismatch")
}

func TestList_OmitsSuggestions(t *testing.T) {
	router := setupTestRouter(t)
	analyzedJob(t, router)
	rec := get(router, "/v1/reconciliations?legal_entity_id=LE-3003", "principal-test")
	var page struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Data) == 0 {
		t.Fatalf("no jobs listed: %s", rec.Body.String())
	}
	for _, j := range page.Data {
		if _, has := j["unmatched_items"]; has {
			t.Fatalf("list items must omit suggestions: %v", j)
		}
	}
}
