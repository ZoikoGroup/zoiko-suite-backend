package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

// A CLOSED pair (matched, then closed once its period was consolidated) is
// eliminated on a re-run exactly as it was while MATCHED.
func TestStartRun_EliminatesClosedIntercompanyEntry(t *testing.T) {
	s := newStubStore()
	tgtJournal := "journal-uk-1"
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0, "1500-IC-Receivable": 500.0},
			"le-uk": {"1000-Cash": 3000.0, "2500-IC-Payable": -500.0},
		},
		intercompanyEntries: []clients.IntercompanyEntry{{
			IntercompanyEntryID: "ic-1", SourceLegalEntityID: "le-us", TargetLegalEntityID: "le-uk",
			SourceJournalID: "journal-us-1", TargetJournalID: &tgtJournal, Amount: "500", CounterpartyAmount: "500",
			FiscalPeriod: strPtr("2024-Q1"), MatchStatus: "CLOSED",
		}},
		journals: map[string]*domain.PostedJournal{
			"journal-us-1": finalized("journal-us-1", "le-us", "2024-Q1", domain.PostedLine{AccountCode: "1500-IC-Receivable", DebitAmount: 500.0}),
			"journal-uk-1": finalized("journal-uk-1", "le-uk", "2024-Q1", domain.PostedLine{AccountCode: "2500-IC-Payable", CreditAmount: 500.0}),
		},
	}
	rr := doReq(newRouter(s, &stubAuthZ{}, cl), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":      "le-group",
		"child_legal_entity_ids":     []string{"le-us", "le-uk"},
		"fiscal_period":              "2024-Q1",
		"target_currency":            "USD",
		"intercompany_account_codes": []string{"1500-IC-Receivable", "2500-IC-Payable"},
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ExceptionCount != 0 {
		t.Fatalf("expected no elimination exceptions, got %d", resp.ExceptionCount)
	}
	for _, snap := range resp.Snapshots {
		if (snap.AccountCode == "1500-IC-Receivable" || snap.AccountCode == "2500-IC-Payable") && snap.ConsolidatedBalance != 0 {
			t.Errorf("CLOSED pair not eliminated: %s = %v", snap.AccountCode, snap.ConsolidatedBalance)
		}
	}
}

// The matched balance is summed exactly. In float64, 0.7 + 0.1 is
// 0.7999999999999999, so a proposal of exactly 0.8 used to be refused as
// exceeding a matched balance of exactly 0.8.
func TestCreateEliminationProposal_MatchedBalanceSummedExactly(t *testing.T) {
	s := newStubStore()
	s.groupHasRun["group-1"] = true
	cl := &stubClients{intercompanyEntries: []clients.IntercompanyEntry{
		{IntercompanyEntryID: "ic-1", Amount: "0.7", MatchStatus: "MATCHED"},
		{IntercompanyEntryID: "ic-2", Amount: "0.1", MatchStatus: "CLOSED"},
	}}
	req := validAdjustmentReq(domain.AdjustmentTypeElimination)
	req["lines"] = []map[string]any{
		{"account_code": "9000", "debit_amount": 0.4},
		{"account_code": "9100", "credit_amount": 0.4},
	}
	resp := doReq(newRouter(s, &stubAuthZ{}, cl), http.MethodPost, "/v1/consolidation/adjustments", req, "preparer-1")
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a proposal equal to the matched balance, got %d: %s", resp.Code, resp.Body.String())
	}

	// One ten-thousandth over is still refused.
	req["lines"] = []map[string]any{
		{"account_code": "9000", "debit_amount": 0.4},
		{"account_code": "9100", "credit_amount": 0.4001},
	}
	resp = doReq(newRouter(s, &stubAuthZ{}, cl), http.MethodPost, "/v1/consolidation/adjustments", req, "preparer-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for a proposal over the matched balance, got %d: %s", resp.Code, resp.Body.String())
	}
}
