package handler_test

// SPEC_DEVIATIONS A2a through the real analyze route: bank-reconciliation-svc
// lines (with their gl_cash_account_code), the governed date tolerance from
// financial-control-svc, and general-ledger-svc's FINALIZED ledger entries,
// all faked at the HTTP boundary. The fakes for bank-reconciliation-svc and
// general-ledger-svc fail the test on any non-GET: proposing a match never
// writes anything anywhere.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

const (
	runCandidates = "44444444-4444-4444-8444-444444444444"
	candidateCash = "1000-Cash"
)

// candidateRun is one run of three lines on cash account 1000-Cash, dated
// 2026-07-01 (the fake's statement date).
func candidateRun() map[string]fakeRun {
	return map[string]fakeRun{runCandidates: {LegalEntityID: "le-1", CashAccount: candidateCash,
		Lines: [][2]string{{"sl-near", "125.50"}, {"sl-far", "300.00"}, {"sl-none", "42.00"}}}}
}

func ledgerEntry(journal, entity, date, debit string) fakeLedgerEntry {
	return fakeLedgerEntry{JournalID: journal, TenantID: "tenant-test", LegalEntityID: entity, AccountCode: candidateCash,
		Currency: "USD", Date: date, Debit: debit}
}

func analyzedItems(t *testing.T, rec *httptest.ResponseRecorder) map[string]domain.ReconciliationSuggestion {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var job domain.ReconciliationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	out := map[string]domain.ReconciliationSuggestion{}
	for _, it := range job.UnmatchedItems {
		out[it.StatementLineID] = it
	}
	return out
}

func TestAnalyze_ProposesAutoMatchAndDateSkewFromTheLedger(t *testing.T) {
	var queries []string
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{
		ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000001", "le-1", "2026-07-02", "125.50"), // 1 day: AUTO_MATCH
		ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000002", "le-1", "2026-07-15", "300"),    // 14 days: DATE_SKEW
		ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000003", "le-2", "2026-07-01", "42.00"),  // other entity: never
	}, &queries)
	router := newTestRouter(t, deps{runs: candidateRun(), ledgerURL: gl.URL})

	items := analyzedItems(t, postAnalyze(router, "le-1", runCandidates))

	near := items["sl-near"]
	if near.Recommendation != domain.RecommendationAutoMatch || near.CandidateSearchOutcome != domain.CandidateWithinDateTolerance ||
		near.CounterpartJournalID != "11111111-aaaa-4aaa-8aaa-000000000001" || near.SuggestionStatus != domain.StatusRecommended {
		t.Fatalf("sl-near: expected an AUTO_MATCH proposal, got %+v", near)
	}
	far := items["sl-far"]
	if far.DiscrepancyType != domain.DiscrepancyDateSkew || far.Recommendation != domain.RecommendationManualReview ||
		far.CounterpartJournalID != "11111111-aaaa-4aaa-8aaa-000000000002" {
		t.Fatalf("sl-far: expected a DATE_SKEW proposal for review, got %+v", far)
	}
	none := items["sl-none"]
	if none.CandidateSearchOutcome != domain.CandidateNone || none.CounterpartJournalID != "" {
		t.Fatalf("sl-none: the other entity's journal must not be proposed; expected NO_CANDIDATE, got %+v", none)
	}
	for id, it := range items {
		if it.DateToleranceVersion != 1 {
			t.Errorf("%s: date tolerance version %d, want the governed policy's 1", id, it.DateToleranceVersion)
		}
	}

	// One read for the whole run, not one per line, scoped as asked.
	if len(queries) != 1 {
		t.Fatalf("expected exactly one ledger read for a three-line run, got %d: %v", len(queries), queries)
	}
	for _, want := range []string{"account_code=1000-Cash", "legal_entity_id=le-1", "transaction_date_from=2026-05-31",
		"transaction_date_to=2026-08-01", "journal_status=FINALIZED", "limit=1000"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("ledger read %q is missing %s", queries[0], want)
		}
	}
}

// A same-amount journal in another currency is not this line's book side.
func TestAnalyze_CurrencyMismatch_IsNotProposed(t *testing.T) {
	runs := candidateRun()
	r := runs[runCandidates]
	r.Currency = "EUR"
	runs[runCandidates] = r
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000001", "le-1", "2026-07-01", "125.50")}, nil)
	items := analyzedItems(t, postAnalyze(newTestRouter(t, deps{runs: runs, ledgerURL: gl.URL}), "le-1", runCandidates))
	if it := items["sl-near"]; it.CounterpartJournalID != "" || it.CandidateSearchOutcome != domain.CandidateNone {
		t.Fatalf("a USD journal was proposed for a EUR line: %+v", it)
	}
}

// general-ledger-svc answering with another entity's journal (a filter
// ignored upstream) fails the analysis closed: nothing is stored or proposed.
func TestAnalyze_LedgerReturnsAnotherEntitysJournal_FailsClosed(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"journal_id":"j-x","tenant_id":"tenant-test","legal_entity_id":"le-2","account_code":"1000-Cash",` +
			`"debit_amount":125.5,"credit_amount":0,"currency_code":"USD","transaction_date":"2026-07-01"}]`))
	}))
	t.Cleanup(gl.Close)
	rec := postAnalyze(newTestRouter(t, deps{runs: candidateRun(), ledgerURL: gl.URL}), "le-1", runCandidates)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyze_LedgerUnavailableOrTruncated_FailsClosed(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		rec := postAnalyze(newTestRouter(t, deps{runs: candidateRun(), ledgerURL: "http://127.0.0.1:1"}), "le-1", runCandidates)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a full page cannot be told from a truncated one", func(t *testing.T) {
		entries := make([]fakeLedgerEntry, 1000)
		for i := range entries {
			entries[i] = ledgerEntry(fmt.Sprintf("11111111-aaaa-4aaa-8aaa-%012d", i), "le-1", "2026-07-01", "1")
		}
		gl := newFakeLedgerServer(t, entries, nil)
		rec := postAnalyze(newTestRouter(t, deps{runs: candidateRun(), ledgerURL: gl.URL}), "le-1", runCandidates)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// The date tolerance is governed: no policy, or one counted in business days
// the platform has no calendar for, refuses the analysis — never a default.
func TestAnalyze_DateToleranceMustBeGoverned(t *testing.T) {
	policy := func(candidate map[string]any) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			items := []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "10", "date_tolerance_days": 0, "date_basis": "CALENDAR"}}
			if r.URL.Query().Get("metric") == domain.MetricCandidateDateTolerance {
				items = nil
				if candidate != nil {
					items = []map[string]any{candidate}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for name, candidate := range map[string]map[string]any{
		"not configured": nil,
		"business days":  {"tolerance_version": 2, "absolute_tolerance": "0", "date_tolerance_days": 2, "date_basis": "BUSINESS"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postAnalyze(newTestRouter(t, deps{runs: candidateRun(), financialControlURL: policy(candidate)}), "le-1", runCandidates)
			if rec.Code != http.StatusFailedDependency || !strings.Contains(rec.Body.String(), domain.MetricCandidateDateTolerance) {
				t.Fatalf("expected 424 naming %s, got %d: %s", domain.MetricCandidateDateTolerance, rec.Code, rec.Body.String())
			}
		})
	}
}

// Proposing is the only thing this service does. Marking an AUTO_MATCH
// proposal EXECUTED with a ledger journal is refused: the match is carried
// out in bank-reconciliation-svc, not posted from here.
func TestAutoMatchProposal_CannotBeExecutedAsAPosting(t *testing.T) {
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000001", "le-1", "2026-07-02", "125.50")}, nil)
	router := newTestRouter(t, deps{runs: candidateRun(), ledgerURL: gl.URL})
	rec := postAnalyze(router, "le-1", runCandidates)
	items := analyzedItems(t, rec)
	var job domain.ReconciliationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	near := items["sl-near"]

	apply := func(body map[string]any, key string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/"+job.ID+"/resolutions/"+near.ID+"/apply", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		withEnvelope(req)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	// A human approves the proposal first (RECOMMENDED -> APPROVED).
	if out := apply(map[string]any{"suggestion_status": "APPROVED"}, "idem-approve"); out.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", out.Code, out.Body.String())
	}
	out := apply(map[string]any{"suggestion_status": "EXECUTED",
		"authoritative_ref": map[string]string{"kind": "GL_JOURNAL", "journal_id": near.CounterpartJournalID}}, "idem-execute")
	if out.Code < 400 || out.Code >= 500 {
		t.Fatalf("expected a 4xx refusal, got %d: %s", out.Code, out.Body.String())
	}
	if !strings.Contains(out.Body.String(), "carried out in bank-reconciliation-svc") {
		t.Fatalf("expected the execution-reference refusal, got %d: %s", out.Code, out.Body.String())
	}
}
