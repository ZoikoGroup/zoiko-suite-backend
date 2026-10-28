package handler_test

// SPEC_DEVIATIONS A2b through the real analyze route: a line no
// general-ledger-svc journal matches is searched for a banking-connector-svc
// NORMALIZED canonical transaction, faked at the HTTP boundary. The fake
// fails the test on any non-GET: proposing a match writes nothing anywhere.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

const canonicalTxnID = "7a1b2c3d-0000-4000-8000-0000000a2b2b"

func canonicalTxn(id, entity, date, amount string) fakeCanonicalTxn {
	return fakeCanonicalTxn{ID: id, TenantID: "tenant-test", LegalEntityID: entity, BankAccountID: "ba-" + runCandidates,
		Currency: "USD", Date: date, Amount: amount}
}

func TestAnalyze_ProposesCanonicalTransactionWhenNoJournalMatches(t *testing.T) {
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{
		ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000001", "le-1", "2026-07-02", "125.50"), // sl-near: journal
	}, nil)
	bc := &fakeBanking{txns: []fakeCanonicalTxn{
		canonicalTxn(canonicalTxnID, "le-1", "2026-07-02", "300.0000"), // sl-far: no journal, this transaction
		// Would also match sl-near; sl-near has a journal and is never searched.
		canonicalTxn("7a1b2c3d-0000-4000-8000-00000000dec0", "le-1", "2026-07-01", "125.5000"),
	}}
	router := newTestRouter(t, deps{runs: candidateRun(), ledgerURL: gl.URL, bankingURL: bc.server(t).URL})
	rec := postAnalyze(router, "le-1", runCandidates)
	items := analyzedItems(t, rec)

	if got := items["sl-near"]; got.CounterpartJournalID != "11111111-aaaa-4aaa-8aaa-000000000001" || got.CounterpartTransactionID != "" {
		t.Fatalf("sl-near: expected the journal only, got %+v", got)
	}
	got := items["sl-far"]
	if got.Recommendation != "AUTO_MATCH" || got.CounterpartTransactionID != canonicalTxnID || got.CounterpartJournalID != "" ||
		got.HeuristicRule != "CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE" || got.SuggestionStatus != "RECOMMENDED" {
		t.Fatalf("sl-far: expected an AUTO_MATCH of the canonical transaction, got %+v", got)
	}
	if got := items["sl-none"]; got.CandidateSearchOutcome != "NO_CANDIDATE" || got.CounterpartTransactionID != "" {
		t.Fatalf("sl-none: expected NO_CANDIDATE, got %+v", got)
	}

	// The proposal is on the wire under its own name.
	var raw struct {
		UnmatchedItems []map[string]any `json:"unmatched_items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	found := false
	for _, it := range raw.UnmatchedItems {
		if it["counterpart_transaction_id"] == canonicalTxnID {
			found = true
		}
	}
	if !found {
		t.Fatalf("counterpart_transaction_id missing from the response: %s", rec.Body.String())
	}

	// One read for the run's bank account, currency and window — not one per line.
	queries := bc.asked()
	if len(queries) != 1 {
		t.Fatalf("expected exactly one canonical read, got %d: %v", len(queries), queries)
	}
	q, _ := url.ParseQuery(queries[0])
	for k, v := range map[string]string{"legal_entity_id": "le-1", "bank_account_id": "ba-" + runCandidates, "currency": "USD",
		"transaction_date_from": "2026-05-31", "transaction_date_to": "2026-08-01", "limit": "5000"} {
		if q.Get(k) != v {
			t.Errorf("canonical read %s = %q, want %q", k, q.Get(k), v)
		}
	}
}

// When every line has a journal result, banking-connector-svc is not called
// at all — even one that would fail.
func TestAnalyze_NoCanonicalReadWhenEveryLineHasAJournalResult(t *testing.T) {
	runs := map[string]fakeRun{runCandidates: {LegalEntityID: "le-1", CashAccount: candidateCash, Lines: [][2]string{{"sl-near", "125.50"}}}}
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{ledgerEntry("11111111-aaaa-4aaa-8aaa-000000000001", "le-1", "2026-07-02", "125.50")}, nil)
	bc := &fakeBanking{status: http.StatusServiceUnavailable}
	items := analyzedItems(t, postAnalyze(newTestRouter(t, deps{runs: runs, ledgerURL: gl.URL, bankingURL: bc.server(t).URL}), "le-1", runCandidates))
	if items["sl-near"].CounterpartJournalID == "" {
		t.Fatalf("expected the journal proposal, got %+v", items["sl-near"])
	}
	if n := len(bc.asked()); n != 0 {
		t.Fatalf("banking-connector-svc was read %d time(s) with nothing to search", n)
	}
}

// A NO_CANDIDATE line's canonical search must complete or nothing is
// analyzed: "could not look" is never "no candidate".
func TestAnalyze_CanonicalReadFailure_FailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		bc   *fakeBanking
		url  string
		want int
	}{
		"unreachable":                       {url: "http://127.0.0.1:1", want: http.StatusServiceUnavailable},
		"page with more behind it":          {bc: &fakeBanking{nextCursor: "YWJj"}, want: http.StatusUnprocessableEntity},
		"bank.transaction.read not granted": {bc: &fakeBanking{status: http.StatusForbidden}, want: http.StatusBadGateway},
		"another tenant's transaction": {bc: &fakeBanking{txns: []fakeCanonicalTxn{func() fakeCanonicalTxn {
			x := canonicalTxn(canonicalTxnID, "le-1", "2026-07-01", "42.0000")
			x.TenantID = "tenant-other" // a filter silently ignored upstream
			return x
		}()}}, want: http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			u := tc.url
			if tc.bc != nil {
				u = tc.bc.server(t).URL
			}
			rec := postAnalyze(newTestRouter(t, deps{runs: candidateRun(), bankingURL: u}), "le-1", runCandidates)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}
