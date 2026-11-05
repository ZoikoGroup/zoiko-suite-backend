package handler_test

// SPEC_DEVIATIONS A2b part 3 through the real analyze route: a journal
// bank-reconciliation-svc already reports as matched or pending-confirmation
// elsewhere must not be proposed again for a different UNMATCHED line.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// newFakeBankRecServerWithClaims behaves exactly like newFakeBankRecServer
// (same run/line fixtures, same read-only enforcement) but additionally
// answers ?journal_ids= with claimed naming every one of the given journal
// ids as already matched elsewhere — the shape
// bankrec.LookupClaimedJournalIDs decodes. Every other route is proxied
// straight through to an inner newFakeBankRecServer, so its fixtures and
// read-only enforcement still apply unchanged.
func newFakeBankRecServerWithClaims(t *testing.T, runs map[string]fakeRun, claimed map[string]bool) *httptest.Server {
	t.Helper()
	inner := newFakeBankRecServer(t, runs)
	innerURL, err := url.Parse(inner.URL)
	if err != nil {
		t.Fatalf("parse inner fake URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(innerURL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statement-lines" && r.URL.Query().Get("journal_ids") != "" {
			w.Header().Set("Content-Type", "application/json")
			var lines []string
			for _, id := range strings.Split(r.URL.Query().Get("journal_ids"), ",") {
				if claimed[id] {
					lines = append(lines, fmt.Sprintf(`{"statement_line_id":"other-line","matched_journal_id":%q}`, id))
				}
			}
			_, _ = w.Write([]byte("[" + strings.Join(lines, ",") + "]"))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnalyze_AlreadyMatchedJournal_IsExcludedFromProposal(t *testing.T) {
	claimedJournal := "11111111-aaaa-4aaa-8aaa-000000000001"
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{
		ledgerEntry(claimedJournal, "le-1", "2026-07-02", "125.50"), // would be AUTO_MATCH alone
	}, nil)
	br := newFakeBankRecServerWithClaims(t, candidateRun(), map[string]bool{claimedJournal: true})
	router := newTestRouter(t, deps{runs: candidateRun(), bankRecURL: br.URL, ledgerURL: gl.URL})

	items := analyzedItems(t, postAnalyze(router, "le-1", runCandidates))

	near := items["sl-near"]
	if near.CandidateSearchOutcome != domain.CandidateNone || near.CounterpartJournalID != "" {
		t.Fatalf("sl-near: expected NO_CANDIDATE once its only qualifying journal is already claimed elsewhere, got %+v", near)
	}
}

// The companion case: an unclaimed journal is proposed exactly as A2a
// already proves, confirming the claims check is not disqualifying every
// journal unconditionally.
func TestAnalyze_UnclaimedJournal_IsStillProposed(t *testing.T) {
	journal := "11111111-aaaa-4aaa-8aaa-000000000001"
	gl := newFakeLedgerServer(t, []fakeLedgerEntry{
		ledgerEntry(journal, "le-1", "2026-07-02", "125.50"),
	}, nil)
	br := newFakeBankRecServerWithClaims(t, candidateRun(), map[string]bool{}) // nothing claimed
	router := newTestRouter(t, deps{runs: candidateRun(), bankRecURL: br.URL, ledgerURL: gl.URL})

	items := analyzedItems(t, postAnalyze(router, "le-1", runCandidates))

	near := items["sl-near"]
	if near.CandidateSearchOutcome != domain.CandidateWithinDateTolerance || near.CounterpartJournalID != journal {
		t.Fatalf("sl-near: expected the unclaimed journal proposed, got %+v", near)
	}
}
