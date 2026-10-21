package domain

import (
	"strings"
	"testing"
)

func period(p string) *string { return &p }

func TestScopePair(t *testing.T) {
	entities := map[string]bool{"le-us": true, "le-uk": true}
	pair := func(src, tgt string, p *string) IntercompanyPair {
		return IntercompanyPair{SourceLegalEntityID: src, TargetLegalEntityID: tgt, FiscalPeriod: p}
	}
	for name, tc := range map[string]struct {
		p    IntercompanyPair
		want PairScope
	}{
		"both in, run period":      {pair("le-us", "le-uk", period("2026-07")), PairInScope},
		"both in, other period":    {pair("le-us", "le-uk", period("2026-08")), PairOutOfScope},
		"target outside the group": {pair("le-us", "le-de", period("2026-07")), PairOutOfScope},
		"source outside the group": {pair("le-de", "le-uk", period("2026-07")), PairOutOfScope},
		"no period recorded":       {pair("le-us", "le-uk", nil), PairNeedsSourcePeriod},
		"empty period recorded":    {pair("le-us", "le-uk", period("")), PairNeedsSourcePeriod},
		"outside group, no period": {pair("le-us", "le-de", nil), PairOutOfScope},
	} {
		if got := ScopePair(tc.p, entities, "2026-07"); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func icPair() IntercompanyPair {
	tgt := "j-uk"
	return IntercompanyPair{
		IntercompanyEntryID: "ic-1", SourceLegalEntityID: "le-us", TargetLegalEntityID: "le-uk",
		SourceJournalID: "j-us", TargetJournalID: &tgt, Amount: "500", CounterpartyAmount: "400",
		FiscalPeriod: period("2026-07"), MatchStatus: "MATCHED",
	}
}

func journal(id, entity string, lines ...PostedLine) *PostedJournal {
	return &PostedJournal{JournalID: id, LegalEntityID: entity, FiscalPeriod: "2026-07", Status: "FINALIZED", Lines: lines}
}

var icAccounts = []string{"1500-IC-REC", "2500-IC-PAY", "4000-IC-REV", "6000-IC-EXP"}

// Only the lines on declared intercompany accounts are eliminated; a
// third-party line carried on the same journal stays in the group balance.
func TestPlanElimination_OnlyIntercompanyLines(t *testing.T) {
	source := journal("j-us", "le-us",
		PostedLine{LineNumber: 1, AccountCode: "1500-IC-REC", DebitAmount: 500},
		PostedLine{LineNumber: 2, AccountCode: "4000-IC-REV", CreditAmount: 500},
		PostedLine{LineNumber: 3, AccountCode: "1000-CASH", DebitAmount: 40},
		PostedLine{LineNumber: 4, AccountCode: "4100-SALES", CreditAmount: 40})
	// A cross-currency pair: the target side is checked against the
	// counterparty amount, not the source amount.
	target := journal("j-uk", "le-uk",
		PostedLine{LineNumber: 1, AccountCode: "6000-IC-EXP", DebitAmount: 400.0005},
		PostedLine{LineNumber: 2, AccountCode: "2500-IC-PAY", CreditAmount: 400.0005})

	lines, reason := PlanElimination(icPair(), source, target, icAccounts, "2026-07")
	if reason != "" {
		t.Fatalf("blocked: %s", reason)
	}
	if len(lines) != 4 {
		t.Fatalf("expected the 4 intercompany lines, got %+v", lines)
	}
	for _, l := range lines {
		if l.AccountCode == "1000-CASH" || l.AccountCode == "4100-SALES" {
			t.Fatalf("eliminated a non-intercompany line: %+v", l)
		}
	}
	if lines[0].Side != EliminationSideSource || lines[0].LineNumber != 1 || lines[0].DebitAmount != "500" || lines[0].Net() != 500 {
		t.Fatalf("first line: %+v", lines[0])
	}
	if lines[3].Side != EliminationSideTarget || lines[3].CreditAmount != "400.0005" || lines[3].Net() != -400.0005 {
		t.Fatalf("last line: %+v", lines[3])
	}
}

// Each blocking rule returns no line at all, so the caller never eliminates
// half a pair.
func TestPlanElimination_Blocked(t *testing.T) {
	good := func() (*PostedJournal, *PostedJournal) {
		return journal("j-us", "le-us", PostedLine{AccountCode: "1500-IC-REC", DebitAmount: 500}),
			journal("j-uk", "le-uk", PostedLine{AccountCode: "2500-IC-PAY", CreditAmount: 400})
	}
	for name, tc := range map[string]struct {
		mutate   func(p *IntercompanyPair, s, t *PostedJournal) []string
		contains string
	}{
		"no accounts declared": {func(_ *IntercompanyPair, _, _ *PostedJournal) []string { return nil }, "declared no intercompany_account_codes"},
		"no target journal": {func(p *IntercompanyPair, _, _ *PostedJournal) []string {
			p.TargetJournalID = nil
			return icAccounts
		}, "no target_journal_id"},
		"target not finalized": {func(_ *IntercompanyPair, _, t *PostedJournal) []string {
			t.Status = "REVERSED"
			return icAccounts
		}, "target journal j-uk is REVERSED"},
		"source in another period": {func(_ *IntercompanyPair, s, _ *PostedJournal) []string {
			s.FiscalPeriod = "2026-06"
			return icAccounts
		}, "source journal j-us is in period 2026-06"},
		"target of another entity": {func(_ *IntercompanyPair, _, t *PostedJournal) []string {
			t.LegalEntityID = "le-de"
			return icAccounts
		}, "belongs to entity le-de"},
		"no intercompany line": {func(_ *IntercompanyPair, s, _ *PostedJournal) []string {
			s.Lines[0].AccountCode = "1000-CASH"
			return icAccounts
		}, "source journal j-us has no line on a declared intercompany account"},
		"intercompany lines disagree with the pair": {func(_ *IntercompanyPair, s, _ *PostedJournal) []string {
			s.Lines[0].DebitAmount = 499.99
			return icAccounts
		}, "intercompany lines total 499.99, not the pair's source amount 500"},
		"no counterparty amount": {func(p *IntercompanyPair, _, _ *PostedJournal) []string {
			p.CounterpartyAmount = ""
			return icAccounts
		}, "no counterparty_amount"},
	} {
		p := icPair()
		s, tg := good()
		accounts := tc.mutate(&p, s, tg)
		lines, reason := PlanElimination(p, s, tg, accounts, "2026-07")
		if lines != nil || !strings.Contains(reason, tc.contains) {
			t.Errorf("%s: lines %+v, reason %q", name, lines, reason)
		}
	}
}

func TestNormalizeAccountCodes(t *testing.T) {
	got, err := NormalizeAccountCodes([]string{" 2500 ", "1500", "2500"})
	if err != nil || len(got) != 2 || got[0] != "1500" || got[1] != "2500" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := NormalizeAccountCodes([]string{"1500", " "}); err == nil {
		t.Fatal("an empty account code was accepted")
	}
	if got, err := NormalizeAccountCodes(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil: %v, %v", got, err)
	}
}
