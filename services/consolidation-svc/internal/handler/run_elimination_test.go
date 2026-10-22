package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

// A2/B9 fixture: le-us and le-uk are the run's children for 2026-07.
//
//	ic-in          le-us -> le-uk, 2026-07            in scope
//	ic-legacy-in   le-us -> le-uk, no period; source journal in 2026-07: in scope
//	ic-legacy-out  le-us -> le-uk, no period; source journal in 2026-06: out
//	ic-outside     le-us -> le-de (not a child)       out
//	ic-other       le-us -> le-uk, 2026-08            out
//
// The source journal of ic-in also carries a third-party sale (cash 40 /
// sales 40) that is not intercompany and must stay in the group balance.
func scopedFixture() *stubClients {
	t := func(s string) *string { return &s }
	pair := func(id, tgtEntity, srcJ, tgtJ, amount string, p *string) clients.IntercompanyEntry {
		return clients.IntercompanyEntry{
			IntercompanyEntryID: id, SourceLegalEntityID: "le-us", TargetLegalEntityID: tgtEntity,
			SourceJournalID: srcJ, TargetJournalID: t(tgtJ), Amount: domain.Decimal(amount),
			CounterpartyAmount: domain.Decimal(amount), FiscalPeriod: p, MatchStatus: "MATCHED",
		}
	}
	line := func(n int, acct string, dr, cr float64) domain.PostedLine {
		return domain.PostedLine{LineNumber: n, AccountCode: acct, DebitAmount: dr, CreditAmount: cr}
	}
	return &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-CASH": 5040, "1500-IC-REC": 700, "4000-IC-REV": -700, "4100-SALES": -40},
			"le-uk": {"1000-CASH": 3000, "6000-IC-EXP": 700, "2500-IC-PAY": -700},
		},
		intercompanyEntries: []clients.IntercompanyEntry{
			pair("ic-in", "le-uk", "j-us-1", "j-uk-1", "500", t("2026-07")),
			pair("ic-legacy-in", "le-uk", "j-us-2", "j-uk-2", "200", nil),
			pair("ic-legacy-out", "le-uk", "j-us-3", "j-uk-3", "90", nil),
			pair("ic-outside", "le-de", "j-us-4", "j-de-4", "60", t("2026-07")),
			pair("ic-other", "le-uk", "j-us-5", "j-uk-5", "30", t("2026-08")),
		},
		journals: map[string]*domain.PostedJournal{
			"j-us-1": finalized("j-us-1", "le-us", "2026-07",
				line(1, "1500-IC-REC", 500, 0), line(2, "4000-IC-REV", 0, 500),
				line(3, "1000-CASH", 40, 0), line(4, "4100-SALES", 0, 40)),
			"j-uk-1": finalized("j-uk-1", "le-uk", "2026-07", line(1, "6000-IC-EXP", 500, 0), line(2, "2500-IC-PAY", 0, 500)),
			"j-us-2": finalized("j-us-2", "le-us", "2026-07", line(1, "1500-IC-REC", 200, 0), line(2, "4000-IC-REV", 0, 200)),
			"j-uk-2": finalized("j-uk-2", "le-uk", "2026-07", line(1, "6000-IC-EXP", 200, 0), line(2, "2500-IC-PAY", 0, 200)),
			"j-us-3": finalized("j-us-3", "le-us", "2026-06", line(1, "1500-IC-REC", 90, 0), line(2, "4000-IC-REV", 0, 90)),
		},
	}
}

func startScopedRun(t *testing.T, s *stubStore, cl *stubClients) domain.ConsolidationRunResponse {
	t.Helper()
	rr := doReq(newRouter(s, &stubAuthZ{}, cl), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":      "le-group",
		"child_legal_entity_ids":     []string{"le-us", "le-uk"},
		"fiscal_period":              "2026-07",
		"target_currency":            "USD",
		"intercompany_account_codes": []string{"1500-IC-REC", "2500-IC-PAY", "4000-IC-REV", "6000-IC-EXP"},
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Only pairs between the run's children in the run's period are eliminated,
// only their intercompany lines are subtracted, and each in-scope pair is
// recorded as its own elimination row.
func TestStartRun_EliminatesOnlyInScopeIntercompanyLines(t *testing.T) {
	s := newStubStore()
	cl := scopedFixture()
	resp := startScopedRun(t, s, cl)

	if resp.ExceptionCount != 0 {
		t.Fatalf("expected no exceptions, got %d", resp.ExceptionCount)
	}
	got := map[string]float64{}
	for _, snap := range resp.Snapshots {
		got[snap.AccountCode] = snap.ConsolidatedBalance
	}
	// ic-in (500) and ic-legacy-in (200) are eliminated from every
	// intercompany account; the 2026-06 legacy pair, the pair with le-de and
	// the 2026-08 pair are not subtracted from this run's balances.
	want := map[string]float64{
		"1000-CASH": 8040, "4100-SALES": -40,
		"1500-IC-REC": 0, "4000-IC-REV": 0, "6000-IC-EXP": 0, "2500-IC-PAY": 0,
	}
	for acct, w := range want {
		if got[acct] != w {
			t.Errorf("%s = %v, want %v", acct, got[acct], w)
		}
	}

	rows := s.eliminations[resp.ConsolidationRunID]
	if len(rows) != 2 {
		t.Fatalf("expected 2 elimination rows (ic-in, ic-legacy-in), got %+v", rows)
	}
	byID := map[string]domain.Elimination{}
	for _, e := range rows {
		byID[e.IntercompanyEntryID] = e
	}
	in, legacy := byID["ic-in"], byID["ic-legacy-in"]
	if in.Outcome != domain.EliminationOutcomeEliminated || in.PeriodBasis != domain.PeriodBasisPair || len(in.Lines) != 4 ||
		in.FiscalPeriod != "2026-07" || len(in.IntercompanyAccountCodes) != 4 {
		t.Fatalf("ic-in row: %+v", in)
	}
	for _, l := range in.Lines {
		if l.AccountCode == "1000-CASH" || l.AccountCode == "4100-SALES" {
			t.Fatalf("a third-party line was eliminated: %+v", l)
		}
	}
	if legacy.Outcome != domain.EliminationOutcomeEliminated || legacy.PeriodBasis != domain.PeriodBasisSourceJournal {
		t.Fatalf("ic-legacy-in row: %+v", legacy)
	}
	if len(resp.Eliminations) != 2 {
		t.Fatalf("the response should carry the run's eliminations, got %+v", resp.Eliminations)
	}

	// Out-of-scope pairs are decided without reading their journals; only
	// the legacy pair's source journal is read, to find its period.
	for _, j := range cl.journalReads {
		switch j {
		case "j-us-4", "j-de-4", "j-us-5", "j-uk-5", "j-uk-3":
			t.Errorf("read journal %s of an out-of-scope pair", j)
		}
	}
}

// A run that cannot read the intercompany pairs cannot know what it failed
// to eliminate, so it fails instead of completing uneliminated.
func TestStartRun_IntercompanyUnavailable_FailsRun(t *testing.T) {
	s := newStubStore()
	cl := scopedFixture()
	cl.intercompanyErr = domain.ErrIntercompanyUnavailable
	rr := doReq(newRouter(s, &stubAuthZ{}, cl), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group", "child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period": "2026-07", "target_currency": "USD",
	}, "principal-1")
	if rr.Code != http.StatusServiceUnavailable || s.completedStatus != "FAILED" {
		t.Fatalf("expected 503 and a FAILED run, got %d / %q: %s", rr.Code, s.completedStatus, rr.Body.String())
	}
	for _, snaps := range s.snapshots {
		if len(snaps) > 0 {
			t.Fatal("a run that could not read its pairs wrote snapshots")
		}
	}
}

// Balances whose eliminations could not be recorded would be unexplainable.
func TestStartRun_EliminationRecordFails_RunFails(t *testing.T) {
	s := newStubStore()
	s.elimErr = domain.ErrIdentityMissing
	rr := doReq(newRouter(s, &stubAuthZ{}, scopedFixture()), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group", "child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period": "2026-07", "target_currency": "USD",
		"intercompany_account_codes": []string{"1500-IC-REC"},
	}, "principal-1")
	if rr.Code != http.StatusServiceUnavailable || s.completedStatus != "FAILED" {
		t.Fatalf("expected 503 and a FAILED run, got %d / %q", rr.Code, s.completedStatus)
	}
}

// Without declared intercompany accounts nothing is guessed: in-scope pairs
// are BLOCKED, visible as exceptions, and the balances keep them.
func TestStartRun_NoDeclaredAccounts_BlocksInScopePairs(t *testing.T) {
	s := newStubStore()
	rr := doReq(newRouter(s, &stubAuthZ{}, scopedFixture()), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group", "child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period": "2026-07", "target_currency": "USD",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ConsolidationRunResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.ExceptionCount != 2 {
		t.Fatalf("expected the 2 in-scope pairs as exceptions, got %d", resp.ExceptionCount)
	}
	for _, e := range s.eliminations[resp.ConsolidationRunID] {
		if e.Outcome != domain.EliminationOutcomeBlocked {
			t.Fatalf("eliminated without declared accounts: %+v", e)
		}
	}
	for _, snap := range resp.Snapshots {
		if snap.AccountCode == "1500-IC-REC" && snap.ConsolidatedBalance != 700 {
			t.Fatalf("receivable changed without declared accounts: %v", snap.ConsolidatedBalance)
		}
	}
}

func TestStartRun_EmptyAccountCode_Rejected(t *testing.T) {
	rr := doReq(newRouter(newStubStore(), &stubAuthZ{}, scopedFixture()), http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group", "child_legal_entity_ids": []string{"le-us"},
		"fiscal_period": "2026-07", "target_currency": "USD",
		"intercompany_account_codes": []string{"1500-IC-REC", ""},
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// ExplainElimination for a run answers from the run's recorded rows, never
// from the pair's current reciprocal balance.
func TestExplainElimination_FromRunRecord(t *testing.T) {
	s := newStubStore()
	cl := scopedFixture()
	resp := startScopedRun(t, s, cl)
	r := newRouter(s, &stubAuthZ{}, cl)

	rr := doReq(r, http.MethodGet, "/v1/consolidation/eliminations/explanation?consolidation_run_id="+resp.ConsolidationRunID+
		"&entity_a=le-uk&entity_b=le-us", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var exp domain.RunEliminationExplanation
	if err := json.NewDecoder(rr.Body).Decode(&exp); err != nil {
		t.Fatal(err)
	}
	if !exp.Eliminated || len(exp.Eliminations) != 2 || exp.FiscalPeriod != "2026-07" || exp.GroupLegalEntityID != "le-group" {
		t.Fatalf("explanation: %+v", exp)
	}
	if cl.reciprocalEntityA != "" {
		t.Fatal("the run answer read the current reciprocal balance")
	}

	// A pair the run never recorded.
	rr = doReq(r, http.MethodGet, "/v1/consolidation/eliminations/explanation?consolidation_run_id="+resp.ConsolidationRunID+
		"&entity_a=le-us&entity_b=le-de", nil, "principal-1")
	_ = json.NewDecoder(rr.Body).Decode(&exp)
	if rr.Code != http.StatusOK || exp.Eliminated || len(exp.Eliminations) != 0 {
		t.Fatalf("unrecorded pair: %d %+v", rr.Code, exp)
	}

	// The run fixes the period.
	rr = doReq(r, http.MethodGet, "/v1/consolidation/eliminations/explanation?consolidation_run_id="+resp.ConsolidationRunID+
		"&entity_a=le-us&entity_b=le-uk&fiscal_period=2026-08", nil, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 run_scope_mismatch, got %d", rr.Code)
	}
	rr = doReq(r, http.MethodGet, "/v1/consolidation/eliminations/explanation?consolidation_run_id=nope&entity_a=le-us&entity_b=le-uk", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	rr = doReq(newRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, cl), http.MethodGet,
		"/v1/consolidation/eliminations/explanation?consolidation_run_id="+resp.ConsolidationRunID+"&entity_a=le-us&entity_b=le-uk", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}

	// ListEliminations for the run.
	rr = doReq(r, http.MethodGet, "/v1/consolidation/runs/"+resp.ConsolidationRunID+"/eliminations", nil, "principal-1")
	var rows []domain.Elimination
	if err := json.NewDecoder(rr.Body).Decode(&rows); err != nil || rr.Code != http.StatusOK || len(rows) != 2 {
		t.Fatalf("list: %d %v %+v", rr.Code, err, rows)
	}
}
