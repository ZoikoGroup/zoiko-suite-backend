package kernel

import (
	"math/big"
	"testing"
	"time"
)

const (
	entUK = "00000000-0000-0000-0000-0000000000a1"
	entUS = "00000000-0000-0000-0000-0000000000b2"
)

func snapLine(acct, net string) SnapshotLine {
	return SnapshotLine{AccountCode: acct, Net: net, Debit: "0", Credit: "0"}
}

func baseCompute() *ConsolCompute {
	return &ConsolCompute{
		Group:        ConsolGroup{GroupID: "g", PresentationCurrency: "GBP", ICTolerance: "0.05"},
		Mapping:      ConsolMapping{Version: 3, AccountMap: map[string]string{"1000": "G-1000", "2000": "G-2000", "1500": "G-1500", "2500": "G-2500", "3000": "G-3000", "4000": "G-4000", "5000": "G-5000"}, ICRoles: map[string]string{"1500": RoleDueFrom, "2500": RoleDueTo, "4000": RoleICRevenue, "5000": RoleICExpense}},
		AccountTypes: map[string]string{"1000": "ASSET", "1500": "ASSET", "2000": "LIABILITY", "2500": "LIABILITY", "3000": "EQUITY", "4000": "REVENUE", "5000": "EXPENSE"},
		Scale:        2,
		Rates: map[string]ConsolRate{
			rateKey("USD", "BS"): {Currency: "USD", Class: "BS", Rate: "0.800000000000", RateID: "r-bs"},
			rateKey("USD", "PL"): {Currency: "USD", Class: "PL", Rate: "0.780000000000", RateID: "r-pl"},
		},
		CTAAccount: "G-CTA", DiffAccount: "G-DIFF", Tolerance: big.NewRat(5, 100),
	}
}

func ukMember(lines []SnapshotLine, pos []ICPosition) ConsolMemberData {
	return ConsolMemberData{Input: ConsolInput{LegalEntityID: entUK, FunctionalCurrency: "GBP", OwnershipPct: "100.0000", Sealed: true}, Lines: lines, Positions: pos}
}

func usMember(pct string, lines []SnapshotLine, pos []ICPosition) ConsolMemberData {
	return ConsolMemberData{Input: ConsolInput{LegalEntityID: entUS, FunctionalCurrency: "USD", OwnershipPct: pct, Sealed: true}, Lines: lines, Positions: pos}
}

func TestTranslationUsesTheRateClassOfTheAccountAndBalancesThroughTheReserve(t *testing.T) {
	c := baseCompute()
	c.Members = []ConsolMemberData{
		ukMember([]SnapshotLine{snapLine("1000", "500.00"), snapLine("3000", "-500.00")}, nil),
		usMember("100", []SnapshotLine{snapLine("1000", "1000.00"), snapLine("2000", "-200.00"), snapLine("3000", "-700.00"), snapLine("4000", "-300.00"), snapLine("5000", "200.00")}, nil),
	}
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	// assets 1000 x 0.80 = 800, liability -200 x 0.80 = -160, equity -700 x 0.80 = -560, revenue -300 x 0.78 = -234, expense 200 x 0.78 = 156
	if pop.CTA[entUS] != "-2.00" {
		t.Fatalf("the US book translated to %v short of balance: %v", pop.CTA, pop.Lines)
	}
	sum := new(big.Rat)
	for _, v := range pop.Translation {
		sum.Add(sum, dec(v))
	}
	if sum.Sign() != 0 {
		t.Fatalf("the translated group book must balance, off by %s: %v", sum.FloatString(2), pop.Translation)
	}
	if pop.Translation["G-1000"] != "1300.00" || pop.Translation["G-CTA"] != "-2.00" {
		t.Fatalf("%v", pop.Translation)
	}
	if len(pop.Rates) != 2 {
		t.Fatalf("both rate classes were used and recorded: %v", pop.Rates)
	}
}

func TestAMissingMappingStopsTheRunAndListsEveryBalance(t *testing.T) {
	c := baseCompute()
	delete(c.Mapping.AccountMap, "2000")
	c.Members = []ConsolMemberData{usMember("100", []SnapshotLine{snapLine("1000", "10.00"), snapLine("2000", "-10.00")}, nil)}
	_, err := c.Compute()
	e, ok := AsException(err)
	if !ok || e.Code != "CONSOLIDATION_MAPPING_MISSING" || len(e.Facts["accounts"].([]map[string]any)) != 1 {
		t.Fatalf("%v", err)
	}
}

func pos(ent, acct, cp, ccy, tx, fn string) ICPosition {
	return ICPosition{Entity: ent, Account: acct, Counterparty: cp, Currency: ccy, TxBalance: tx, Functional: fn}
}

func TestPairStatuses(t *testing.T) {
	roles := map[string]string{"1500": RoleDueFrom, "2500": RoleDueTo}
	cases := []struct {
		name   string
		uk, us []ICPosition
		want   string
	}{
		{"matched", []ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")}, []ICPosition{pos(entUS, "2500", entUK, "USD", "-100.00", "-100.00")}, ICMatched},
		{"amount", []ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")}, []ICPosition{pos(entUS, "2500", entUK, "USD", "-90.00", "-90.00")}, ICAmountDifference},
		{"currency", []ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")}, []ICPosition{pos(entUS, "2500", entUK, "EUR", "-100.00", "-100.00")}, ICCurrencyDifference},
		{"unmatched", []ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")}, nil, ICUnmatched},
	}
	for _, tc := range cases {
		pairs, excs := PairPositions([]ConsolMemberData{ukMember(nil, tc.uk), usMember("100", nil, tc.us)}, roles)
		if len(excs) != 0 || len(pairs) != 1 || pairs[0].Status != tc.want {
			t.Fatalf("%s: %+v %v", tc.name, pairs, excs)
		}
	}
}

func TestExternalAndUnattributedBalancesAreReportedNotPaired(t *testing.T) {
	roles := map[string]string{"1500": RoleDueFrom}
	pairs, excs := PairPositions([]ConsolMemberData{ukMember(nil, []ICPosition{
		pos(entUK, "1500", "", "GBP", "5.00", "5.00"), pos(entUK, "1500", "00000000-0000-0000-0000-0000000000ff", "GBP", "7.00", "7.00")})}, roles)
	if len(pairs) != 0 || len(excs) != 2 || excs[0].Code != "IC_COUNTERPARTY_MISSING" || !excs[0].Blocking || excs[1].Code != "IC_COUNTERPARTY_OUTSIDE_GROUP" || excs[1].Blocking {
		t.Fatalf("%v %v", pairs, excs)
	}
}

func icCompute(ukPos, usPos []ICPosition) *ConsolCompute {
	c := baseCompute()
	c.Members = []ConsolMemberData{
		ukMember([]SnapshotLine{snapLine("1500", "80.00"), snapLine("1000", "-80.00")}, ukPos),
		usMember("100", []SnapshotLine{snapLine("2500", "-100.00"), snapLine("1000", "100.00")}, usPos),
	}
	return c
}

func TestMatchedPairIsEliminatedAndTheTranslatedDifferenceStaysVisible(t *testing.T) {
	// UK holds a receivable it booked at 80.00 GBP; the US payable of 100 USD translates to 80.00 at the closing rate
	pop, err := icCompute(
		[]ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")},
		[]ICPosition{pos(entUS, "2500", entUK, "USD", "-100.00", "-100.00")}).Compute()
	if err != nil {
		t.Fatal(err)
	}
	if len(pop.Pairs) != 1 || !pop.Pairs[0].Eliminated || pop.Pairs[0].Status != ICMatched {
		t.Fatalf("%+v", pop.Pairs)
	}
	if pop.Elimination["G-1500"] != "-80.00" || pop.Elimination["G-2500"] != "80.00" {
		t.Fatalf("both sides come out: %v", pop.Elimination)
	}
	if _, left := pop.Consolidated["G-1500"]; left {
		t.Fatalf("the intercompany receivable is gone from the group: %v", pop.Consolidated)
	}
	if pop.BlockingCount() != 0 {
		t.Fatalf("%v", pop.Exceptions)
	}
}

func TestResidualWithinToleranceGoesToTheDifferenceAccountAndBeyondItBlocksThePair(t *testing.T) {
	// the UK carries 80.03 where the translation of 100 USD is 80.00: a 0.03 difference, inside the 0.05 tolerance
	pop, err := icCompute(
		[]ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.03")},
		[]ICPosition{pos(entUS, "2500", entUK, "USD", "-100.00", "-100.00")}).Compute()
	if err != nil {
		t.Fatal(err)
	}
	if !pop.Pairs[0].Eliminated || pop.Elimination["G-DIFF"] != "0.03" {
		t.Fatalf("%+v %v", pop.Pairs[0], pop.Elimination)
	}
	sum := new(big.Rat)
	for _, v := range pop.Elimination {
		sum.Add(sum, dec(v))
	}
	if sum.Sign() != 0 {
		t.Fatalf("an elimination journal balances: %v", pop.Elimination)
	}

	pop, err = icCompute(
		[]ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "81.00")},
		[]ICPosition{pos(entUS, "2500", entUK, "USD", "-100.00", "-100.00")}).Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.Pairs[0].Eliminated || pop.Pairs[0].Status != ICBlocked || len(pop.Elimination) != 0 || pop.BlockingCount() != 1 {
		t.Fatalf("%+v %v %v", pop.Pairs[0], pop.Elimination, pop.Exceptions)
	}
}

func TestAnUnmatchedPairIsNotEliminatedAndBlocksCertification(t *testing.T) {
	pop, err := icCompute([]ICPosition{pos(entUK, "1500", entUS, "USD", "100.00", "80.00")}, nil).Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.Pairs[0].Eliminated || pop.Pairs[0].Status != ICUnmatched || len(pop.Elimination) != 0 {
		t.Fatalf("%+v", pop.Pairs[0])
	}
	if pop.BlockingCount() != 1 || pop.Exceptions[0].Code != "IC_PAIR_UNMATCHED" {
		t.Fatalf("%v", pop.Exceptions)
	}
}

func TestProfitAndLossPairsEliminateAtTheAverageRate(t *testing.T) {
	c := baseCompute()
	c.Members = []ConsolMemberData{
		ukMember([]SnapshotLine{snapLine("4000", "-780.00"), snapLine("1000", "780.00")}, []ICPosition{pos(entUK, "4000", entUS, "USD", "-1000.00", "-780.00")}),
		usMember("100", []SnapshotLine{snapLine("5000", "1000.00"), snapLine("1000", "-1000.00")}, []ICPosition{pos(entUS, "5000", entUK, "USD", "1000.00", "1000.00")}),
	}
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	if !pop.Pairs[0].Eliminated || pop.Pairs[0].Class != ICClassProfitLoss {
		t.Fatalf("%+v", pop.Pairs[0])
	}
	if _, ok := pop.Consolidated["G-4000"]; ok {
		t.Fatalf("intercompany revenue and expense cancel: %v", pop.Consolidated)
	}
	if _, ok := pop.Consolidated["G-5000"]; ok {
		t.Fatalf("%v", pop.Consolidated)
	}
}

func TestNonControllingInterestIsCalculatedFromNetAssetsAndOwnership(t *testing.T) {
	c := baseCompute()
	c.Members = []ConsolMemberData{usMember("80", []SnapshotLine{snapLine("1000", "1000.00"), snapLine("2000", "-400.00"), snapLine("3000", "-600.00")}, nil)}
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	// net assets 600 USD x 0.80 = 480.00; the minority holds 20%
	if len(pop.NCI) != 1 || pop.NCI[0].NetAssets != "480.00" || pop.NCI[0].NCIAmount != "96.00" || pop.NCI[0].NCIPct != "20.0000" {
		t.Fatalf("%+v", pop.NCI)
	}
}

func TestPostedDeltaIsTheChangeFromThePriorRun(t *testing.T) {
	got := PostedDelta(map[string]string{"A": "100.00", "B": "-40.00"}, map[string]string{"A": "60.00", "C": "5.00", "B": "-40.00"}, 2)
	if len(got) != 2 || got["A"] != "40.00" || got["C"] != "-5.00" {
		t.Fatalf("%v", got)
	}
}

func TestMembershipThatChangedInsideThePeriodBlocks(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	pstart, asOf := day("2026-03-01"), day("2026-03-31")
	all := []ConsolMember{{MemberID: "1", LegalEntityID: entUK, EffectiveFrom: "2025-01-01", OwnershipPct: "100"}}
	in, excs := activeMembers(all, pstart, asOf)
	if len(in) != 1 || len(excs) != 0 {
		t.Fatalf("%v %v", in, excs)
	}
	// joined on the 10th
	in, excs = activeMembers([]ConsolMember{{MemberID: "2", LegalEntityID: entUS, EffectiveFrom: "2026-03-10", OwnershipPct: "100"}}, pstart, asOf)
	if len(in) != 1 || len(excs) != 1 || excs[0].Code != "MEMBERSHIP_CHANGED_IN_PERIOD" || !excs[0].Blocking {
		t.Fatalf("%v %v", in, excs)
	}
	// 80% until the 15th, 60% after
	in, excs = activeMembers([]ConsolMember{
		{MemberID: "3", LegalEntityID: entUS, EffectiveFrom: "2025-01-01", EffectiveTo: "2026-03-15", OwnershipPct: "80"},
		{MemberID: "4", LegalEntityID: entUS, EffectiveFrom: "2026-03-15", OwnershipPct: "60"}}, pstart, asOf)
	codes := map[string]bool{}
	for _, e := range excs {
		codes[e.Code] = true
	}
	if len(in) != 1 || in[0].MemberID != "4" || !codes["OWNERSHIP_CHANGED_IN_PERIOD"] {
		t.Fatalf("%v %v", in, excs)
	}
	// left the group before the period began
	in, excs = activeMembers([]ConsolMember{{MemberID: "5", LegalEntityID: entUS, EffectiveFrom: "2025-01-01", EffectiveTo: "2026-02-01", OwnershipPct: "100"}}, pstart, asOf)
	if len(in) != 0 || len(excs) != 0 {
		t.Fatalf("%v %v", in, excs)
	}
}

func TestRunHashChangesWithAnyFigure(t *testing.T) {
	p := &ConsolPopulation{Consolidated: map[string]string{"A": "1.00"}}
	h1 := p.Hash("g", "2026-03", "2026-03-31")
	p.Consolidated["A"] = "1.01"
	if h1 == p.Hash("g", "2026-03", "2026-03-31") {
		t.Fatal("the hash must cover the figures")
	}
}

func TestGroupBookDifferencesFindsWhatTheRunDoesNotSay(t *testing.T) {
	lines := []SnapshotLine{snapLine("A", "10.00"), snapLine("B", "-10.00"), snapLine("C", "0.00")}
	if d := groupBookDifferences(lines, map[string]string{"A": "10.00", "B": "-10.00"}); len(d) != 0 {
		t.Fatalf("%v", d)
	}
	d := groupBookDifferences(lines, map[string]string{"A": "10.00", "D": "5.00"})
	if len(d) != 2 || d[0]["account_code"] != "B" || d[1]["account_code"] != "D" {
		t.Fatalf("%v", d)
	}
}
