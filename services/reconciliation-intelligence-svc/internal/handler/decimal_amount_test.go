package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

const runExact = "44444444-4444-4444-8444-444444444444"

// ZS-DATA-001 D06: bank-reconciliation-svc sends amount as a bare JSON
// number. It is decoded as its decimal text, never through float64 on this
// side, so a 14-digit amount float64 cannot hold survives to the suggestion
// exactly.
func TestAnalyze_UpstreamAmountKeptExactly(t *testing.T) {
	router := newTestRouter(t, deps{runs: map[string]fakeRun{
		runExact: {LegalEntityID: "LE-1", Lines: [][2]string{{"sl-big", "90071992547409.93"}}},
	}})

	rec := postAnalyze(router, "LE-1", runExact)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if got := job.UnmatchedItems[0].AmountA; got != "90071992547409.93" {
		t.Fatalf("amount_a = %s, want 90071992547409.93", got)
	}
}

// An upstream amount the NUMERIC(18,4) columns cannot hold exactly (here a
// float artifact) is refused, not rounded, and nothing is stored.
func TestAnalyze_UpstreamAmountInexact_RefusedNoJob(t *testing.T) {
	memStore := store.NewMemoryStore()
	router := newTestRouter(t, deps{store: memStore, runs: map[string]fakeRun{
		runExact: {LegalEntityID: "LE-1", Lines: [][2]string{{"sl-x", "0.30000000000000004"}}},
	}})

	rec := postAnalyze(router, "LE-1", runExact)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if jobs, _ := memStore.ListJobs(t.Context(), "tenant-test", "LE-1", "", ""); len(jobs) != 0 {
		t.Fatalf("a refused analysis must not create a job, found %d", len(jobs))
	}
}
