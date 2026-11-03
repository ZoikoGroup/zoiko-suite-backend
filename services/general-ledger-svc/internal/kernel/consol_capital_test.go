package kernel

import (
	"math/big"
	"testing"
)

func capitalCompute(invested string, pct string) *ConsolCompute {
	c := baseCompute()
	c.Mapping.AccountMap["1600"] = "G-1600"
	c.AccountTypes["1600"] = "ASSET"
	c.GoodwillAccount, c.NCIAccount = "G-GW", "G-NCI"
	uk := ukMember([]SnapshotLine{snapLine("1600", invested), snapLine("1000", "-"+invested)}, nil)
	us := usMember(pct, []SnapshotLine{snapLine("1000", "5000.00"), snapLine("3000", "-5000.00")}, nil)
	us.Input.InvestorEntityID, us.Input.InvestmentAccount, us.Input.EquityAccounts = entUK, "1600", []string{"3000"}
	c.Members = []ConsolMemberData{uk, us}
	return c
}

func sumOf(m map[string]string) *big.Rat {
	s := new(big.Rat)
	for _, v := range m {
		s.Add(s, dec(v))
	}
	return s
}

func TestCapitalConsolidationEliminatesTheInvestmentAndJournalsTheMinority(t *testing.T) {
	// the US equity of 5000 USD is 4000 GBP at the closing rate; the group holds 80% of it, which is 3200
	pop, err := capitalCompute("3200.00", "80").Compute()
	if err != nil {
		t.Fatal(err)
	}
	e := pop.Elimination
	if e["G-1600"] != "-3200.00" || e["G-3000"] != "4000.00" || e["G-NCI"] != "-800.00" {
		t.Fatalf("%v", e)
	}
	if _, has := e["G-GW"]; has {
		t.Fatalf("paying exactly the share of equity leaves no goodwill: %v", e)
	}
	if sumOf(e).Sign() != 0 {
		t.Fatalf("the elimination balances: %v", e)
	}
	if len(pop.Capital) != 1 || pop.Capital[0].NCI != "800.00" || pop.Capital[0].Goodwill != "0.00" {
		t.Fatalf("%+v", pop.Capital)
	}
	if _, has := pop.Consolidated["G-1600"]; has {
		t.Fatalf("the investment is gone from the group: %v", pop.Consolidated)
	}
	if _, has := pop.Consolidated["G-3000"]; has {
		t.Fatalf("the subsidiary share capital is gone from the group: %v", pop.Consolidated)
	}
	if pop.Consolidated["G-NCI"] != "-800.00" || pop.Consolidated["G-1000"] != "800.00" {
		t.Fatalf("%v", pop.Consolidated)
	}
}

func TestPayingMoreThanTheShareOfEquityIsGoodwillAndLessIsABargain(t *testing.T) {
	pop, err := capitalCompute("3500.00", "80").Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.Elimination["G-GW"] != "300.00" || sumOf(pop.Elimination).Sign() != 0 || pop.Capital[0].Goodwill != "300.00" {
		t.Fatalf("%v", pop.Elimination)
	}
	pop, err = capitalCompute("3000.00", "80").Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.Elimination["G-GW"] != "-200.00" || sumOf(pop.Elimination).Sign() != 0 {
		t.Fatalf("a bargain is a credit: %v", pop.Elimination)
	}
	// a wholly owned subsidiary has no minority
	pop, err = capitalCompute("4000.00", "100").Compute()
	if err != nil {
		t.Fatal(err)
	}
	if _, has := pop.Elimination["G-NCI"]; has || pop.Capital[0].NCI != "0.00" {
		t.Fatalf("%v", pop.Elimination)
	}
}

func TestCapitalConsolidationReportsWhatItCannotEliminate(t *testing.T) {
	c := capitalCompute("3200.00", "80")
	c.Members[1].Input.InvestorEntityID = "00000000-0000-0000-0000-0000000000ff"
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.BlockingCount() != 1 || pop.Exceptions[0].Code != "CAPITAL_INVESTOR_NOT_A_MEMBER" || len(pop.Capital) != 0 {
		t.Fatalf("%v", pop.Exceptions)
	}
	c = capitalCompute("3200.00", "80")
	c.Members[0].Lines = []SnapshotLine{snapLine("1000", "0.00")}
	pop, _ = c.Compute()
	if pop.BlockingCount() != 1 || pop.Exceptions[0].Code != "CAPITAL_INVESTMENT_MISSING" {
		t.Fatalf("%v", pop.Exceptions)
	}
	c = capitalCompute("3200.00", "80")
	delete(c.Mapping.AccountMap, "1600")
	if _, err := c.Compute(); err == nil {
		t.Fatal("an unmapped investment account stops the run")
	}
}

func TestApprovedGroupAdjustmentsAreInTheConsolidatedFigures(t *testing.T) {
	c := baseCompute()
	c.Members = []ConsolMemberData{ukMember([]SnapshotLine{snapLine("1000", "500.00"), snapLine("3000", "-500.00")}, nil)}
	c.Adjustments = []ConsolAdjustment{
		{AdjustmentID: "a1", FiscalPeriod: "2026-03", PolicyRef: "P-1", EvidenceRef: "E-1", Lines: []AdjustmentLine{{AccountCode: "G-1000", Amount: "-40.00"}, {AccountCode: "G-5000", Amount: "40.00"}}},
		{AdjustmentID: "a2", FiscalPeriod: "2026-03", PolicyRef: "P-2", EvidenceRef: "E-2", Lines: []AdjustmentLine{{AccountCode: "G-1000", Amount: "10.00"}, {AccountCode: "G-4000", Amount: "-10.00"}}},
	}
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	if pop.Consolidated["G-1000"] != "470.00" || pop.Consolidated["G-5000"] != "40.00" || pop.Consolidated["G-4000"] != "-10.00" || pop.Adjustment["G-1000"] != "-30.00" {
		t.Fatalf("%v %v", pop.Consolidated, pop.Adjustment)
	}
	if len(pop.Adjustments) != 2 || pop.Adjustments[0].EvidenceRef != "E-1" {
		t.Fatalf("the run records which adjustments it applied: %v", pop.Adjustments)
	}
	if sumOf(pop.Consolidated).Sign() != 0 {
		t.Fatalf("balanced adjustments keep the group book balanced: %v", pop.Consolidated)
	}
}

// Equity translated at its historical rate (IAS 21.39(c)): the share capital stays at the rate of the day it arose, and the
// difference to the closing rate is the translation reserve, not a retranslated equity.
func TestEquityAtItsHistoricalRateStaysAtThatRateAndTheReserveTakesTheDifference(t *testing.T) {
	c := capitalCompute("3000.00", "80")
	c.Rates[rateKey("USD", EquityRateClass("2025-01-01"))] = ConsolRate{Currency: "USD", Class: "EQ", Policy: "SPOT_ON_DATE", Rate: "0.750000000000", RateID: "r-hist", AsOf: "2025-01-01"}
	c.Members[1].Input.EquityRateDate = "2025-01-01"
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	// 5000 USD of capital was 3750 GBP on the day: the group holds 80% = 3000, so the investment is eliminated with no goodwill
	if pop.Elimination["G-3000"] != "3750.00" || pop.Elimination["G-1600"] != "-3000.00" || pop.Elimination["G-NCI"] != "-750.00" {
		t.Fatalf("%v", pop.Elimination)
	}
	if _, has := pop.Elimination["G-GW"]; has {
		t.Fatalf("no goodwill at the historical rate: %v", pop.Elimination)
	}
	// the other balance (5000 USD of cash) is at the closing rate, so the member no longer balances: 4000 - 3750 = 250 into the reserve
	if pop.CTA[entUS] != "-250.00" {
		t.Fatalf("reserve %v", pop.CTA)
	}
	var eq *ConsolLine
	for i := range pop.Lines {
		if pop.Lines[i].LocalAccount == "3000" && pop.Lines[i].LegalEntityID == entUS {
			eq = &pop.Lines[i]
		}
	}
	if eq == nil || eq.Class != "EQ" || eq.Rate != "0.750000000000" || eq.Translated != "-3750.00" {
		t.Fatalf("%+v", eq)
	}
	var recorded bool
	for _, r := range pop.Rates {
		recorded = recorded || (r.Class == "EQ" && r.AsOf == "2025-01-01")
	}
	if !recorded {
		t.Fatalf("the rate the run used is part of its record: %+v", pop.Rates)
	}
	if sumOf(pop.Consolidated).Sign() != 0 {
		t.Fatalf("the group still balances: %v", pop.Consolidated)
	}
}

func TestAnEquityAccountNotNamedForTheMemberKeepsTheClosingRate(t *testing.T) {
	c := capitalCompute("3200.00", "80")
	c.Mapping.AccountMap["3100"], c.AccountTypes["3100"] = "G-3100", "EQUITY"
	c.Members[1].Lines = append(c.Members[1].Lines, snapLine("3100", "-100.00"), snapLine("1000", "100.00"))
	c.Rates[rateKey("USD", EquityRateClass("2025-01-01"))] = ConsolRate{Currency: "USD", Class: "EQ", Rate: "0.750000000000"}
	c.Members[1].Input.EquityRateDate = "2025-01-01"
	pop, err := c.Compute()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range pop.Lines {
		switch {
		case l.LegalEntityID == entUS && l.LocalAccount == "3000" && l.Class != "EQ":
			t.Fatalf("the named account is historical: %+v", l)
		case l.LegalEntityID == entUS && l.LocalAccount == "3100" && (l.Class != "BS" || l.Rate != "0.800000000000"):
			t.Fatalf("retained earnings and the like stay at the closing rate: %+v", l)
		}
	}
}

func TestAMissingHistoricalRateStopsTheRun(t *testing.T) {
	c := capitalCompute("3000.00", "80")
	c.Members[1].Input.EquityRateDate = "2025-01-01"
	if _, err := c.Compute(); err == nil {
		t.Fatal("a historical rate that was not resolved must stop the run, never fall back to closing")
	} else if e, ok := AsException(err); !ok || e.Code != "CONSOLIDATION_RATE_UNAVAILABLE" {
		t.Fatalf("%v", err)
	}
}
