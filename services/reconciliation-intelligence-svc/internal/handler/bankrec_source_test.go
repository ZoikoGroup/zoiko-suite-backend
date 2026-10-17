package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// A1: analysis reads bank-reconciliation-svc's UNMATCHED lines by run id
// instead of trusting transactions posted in the body.

func postAnalyzeRaw(router http.Handler, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func postAnalyze(router http.Handler, legalEntityID, runID string) *httptest.ResponseRecorder {
	return postAnalyzeRaw(router, analyzeBody(legalEntityID, runID))
}

// A caller still sending the pre-A1 body is told the transactions are no
// longer read — they are not silently dropped.
func TestAnalyze_OldPushedTransactionsBody_Refused(t *testing.T) {
	router := setupTestRouter(t)
	rec := postAnalyzeRaw(router, []byte(`{"legal_entity_id":"le-1","job_name":"j","bank_reconciliation_run_id":"`+runSmall+`",`+
		`"transactions_a":[{"ref_id":"TX-1","amount":"5.00"}]}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "transactions_a") {
		t.Fatalf("expected 400 naming transactions_a, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyze_RunIDValidated(t *testing.T) {
	router := setupTestRouter(t)
	for _, id := range []string{"", "not-a-uuid", "../statement-lines"} {
		if rec := postAnalyze(router, "le-1", id); rec.Code != http.StatusBadRequest {
			t.Errorf("run id %q: expected 400, got %d", id, rec.Code)
		}
	}
}

// The job and every suggestion record the run and statement line they are
// about. Match counters are omitted, not reported as 0 / 0%: every line is
// UNMATCHED by definition and match state is bank-reconciliation-svc's.
func TestAnalyze_SuggestionsTraceableAndNoFabricatedRate(t *testing.T) {
	router := setupTestRouter(t)
	rec := postAnalyze(router, "LE-3003", runLE3003)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"matched_count", "reconciliation_rate", "source_system_b"} {
		if _, present := raw[k]; present {
			t.Errorf("%s present on a run-sourced job (%s); it is not measured here", k, raw[k])
		}
	}

	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if job.BankReconciliationRunID != runLE3003 || job.SourceSystemA != domain.SourceBankStatement {
		t.Fatalf("job source = %q / %s", job.BankReconciliationRunID, job.SourceSystemA)
	}
	want := map[string]domain.ResolutionRecommendation{"sl-3003-a": domain.RecommendationManualReview, "sl-3003-b": domain.RecommendationWriteOff}
	for _, it := range job.UnmatchedItems {
		if it.BankReconciliationRunID != runLE3003 {
			t.Errorf("item %s run = %q", it.StatementLineID, it.BankReconciliationRunID)
		}
		if rec, ok := want[it.StatementLineID]; !ok || it.Recommendation != rec {
			t.Errorf("item %q: recommendation %s", it.StatementLineID, it.Recommendation)
		}
		delete(want, it.StatementLineID)
	}
	if len(want) != 0 {
		t.Fatalf("lines with no suggestion: %v", want)
	}
}

// No UNMATCHED lines is a real answer: a job with nothing to suggest.
func TestAnalyze_EmptyPopulation_JobWithNoSuggestions(t *testing.T) {
	const runClean = "55555555-5555-4555-8555-555555555555"
	router := newTestRouter(t, deps{runs: map[string]fakeRun{runClean: {LegalEntityID: "le-1"}}})
	rec := postAnalyze(router, "le-1", runClean)
	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if rec.Code != http.StatusCreated || job.TotalProcessedCount != 0 || len(job.UnmatchedItems) != 0 {
		t.Fatalf("expected 201 with 0 lines/0 suggestions, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Every refusal leaves no job behind.
func TestAnalyze_SourceFailures_RefusedNoJob(t *testing.T) {
	const (
		runOther      = "66666666-6666-4666-8666-666666666666"
		runSuperseded = "77777777-7777-4777-8777-777777777777"
		runHuge       = "88888888-8888-4888-8888-888888888888"
	)
	huge := make([][2]string, 1000)
	for i := range huge {
		huge[i] = [2]string{fmt.Sprintf("sl-%d", i), "1"}
	}
	runs := map[string]fakeRun{
		runOther:      {LegalEntityID: "le-someone-else", Lines: [][2]string{{"sl-o", "1"}}},
		runSuperseded: {LegalEntityID: "le-1", Status: "SUPERSEDED", SupersededBy: runSmall},
		runHuge:       {LegalEntityID: "le-1", Lines: huge},
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"envelope_incomplete"}`))
	}))
	t.Cleanup(refusing.Close)

	cases := []struct {
		name, runID, bankRecURL string
		want                    int
	}{
		{"unknown run", "99999999-9999-4999-8999-999999999999", "", http.StatusNotFound},
		// Authorized for le-1, asked about another entity's run.
		{"run of another legal entity", runOther, "", http.StatusForbidden},
		{"superseded run", runSuperseded, "", http.StatusConflict},
		// A full page cannot be proven complete.
		{"population at the page cap", runHuge, "", http.StatusUnprocessableEntity},
		{"bank-reconciliation-svc down", runSmall, "http://127.0.0.1:1", http.StatusServiceUnavailable},
		// A refusal is this request's defect, not an outage.
		{"bank-reconciliation-svc refuses", runSmall, refusing.URL, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			memStore := store.NewMemoryStore()
			router := newTestRouter(t, deps{runs: runs, store: memStore, bankRecURL: tc.bankRecURL})
			rec := postAnalyze(router, "le-1", tc.runID)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
			if jobs, _ := memStore.ListJobs(t.Context(), "tenant-test", "le-1", "", ""); len(jobs) != 0 {
				t.Fatalf("a refused analysis created %d job(s)", len(jobs))
			}
		})
	}
}
