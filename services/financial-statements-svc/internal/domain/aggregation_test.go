package domain

import "testing"

func acct(code, name string, cat AccountCategory, bal string) SnapshotContentAccount {
	return SnapshotContentAccount{AccountCode: code, AccountName: name, Category: cat, Balance: bal}
}

func mustBuild(t *testing.T, version string, accounts ...SnapshotContentAccount) []StatementFact {
	t.Helper()
	facts, err := BuildFacts(SnapshotContent{Currency: "GBP", Accounts: accounts}, version)
	if err != nil {
		t.Fatalf("BuildFacts(%s): %v", version, err)
	}
	return facts
}

func byName(facts []StatementFact) map[string][]StatementFact {
	out := map[string][]StatementFact{}
	for _, f := range facts {
		out[f.AccountName] = append(out[f.AccountName], f)
	}
	return out
}

func TestBuildFactsV2_SameNameDifferentCodes_MergeIntoOneLine(t *testing.T) {
	facts := mustBuild(t, MappingVersionV2,
		acct("1010", "Cash", CategoryAsset, "250.10"),
		acct("1000", "Cash", CategoryAsset, "500.005"),
		acct("2000", "Payables", CategoryLiability, "750.105"),
	)
	if len(facts) != 2 {
		t.Fatalf("expected Cash merged into one line plus Payables, got %d facts: %+v", len(facts), facts)
	}
	cash := byName(facts)["Cash"]
	if len(cash) != 1 {
		t.Fatalf("expected exactly one Cash line, got %+v", cash)
	}
	c := cash[0]
	if c.Amount != "750.105" {
		t.Errorf("expected exact sum 750.105, got %s", c.Amount)
	}
	if c.AccountCode != "1000" {
		t.Errorf("expected the lowest member code 1000 to represent the line, got %s", c.AccountCode)
	}
	if len(c.SourceAccounts) != 2 || c.SourceAccounts[0].AccountCode != "1000" || c.SourceAccounts[1].AccountCode != "1010" {
		t.Fatalf("expected both source accounts in code order, got %+v", c.SourceAccounts)
	}
	if c.SourceAccounts[0].Balance != "500.005" || c.SourceAccounts[1].Balance != "250.10" {
		t.Errorf("source balances must be kept exactly as the snapshot stated them, got %+v", c.SourceAccounts)
	}
}

func TestBuildFactsV2_DifferentNamesStaySeparate(t *testing.T) {
	facts := mustBuild(t, MappingVersionV2,
		acct("1000", "Cash", CategoryAsset, "1"),
		acct("1001", "Cash ", CategoryAsset, "1"), // exact match only: trailing space differs
		acct("1002", "cash", CategoryAsset, "1"),  // and case differs
	)
	if len(facts) != 3 {
		t.Fatalf("expected three separate lines for three distinct names, got %+v", facts)
	}
}

func TestBuildFactsV2_SameNameDifferentCategory_NotMerged(t *testing.T) {
	// Merging these would move 40 from liabilities into assets (or vice
	// versa) and change the tie-out — category is part of the line key.
	facts := mustBuild(t, MappingVersionV2,
		acct("1900", "Suspense", CategoryAsset, "40"),
		acct("2900", "Suspense", CategoryLiability, "40"),
	)
	if len(facts) != 2 {
		t.Fatalf("expected ASSET and LIABILITY Suspense as two lines, got %+v", facts)
	}
}

func TestBuildFactsV2_UnnamedAccountsNeverMerge(t *testing.T) {
	facts := mustBuild(t, MappingVersionV2,
		acct("1000", "", CategoryAsset, "1"),
		acct("1001", "", CategoryAsset, "2"),
	)
	if len(facts) != 2 {
		t.Fatalf("expected two unnamed accounts to stay separate, got %+v", facts)
	}
}

func TestBuildFactsV1_StillOneFactPerAccount(t *testing.T) {
	facts := mustBuild(t, MappingVersionV1,
		acct("1000", "Cash", CategoryAsset, "1"),
		acct("1010", "Cash", CategoryAsset, "2"),
	)
	if len(facts) != 2 || facts[0].SourceAccounts != nil {
		t.Fatalf("V1 must keep one fact per account with no recorded sources, got %+v", facts)
	}
}

func TestBuildFacts_UnknownMappingVersionRefused(t *testing.T) {
	_, err := BuildFacts(SnapshotContent{Currency: "GBP", Accounts: []SnapshotContentAccount{acct("1000", "Cash", CategoryAsset, "1")}}, "v9-made-up")
	if de, ok := AsError(err); !ok || de.Code != CodeValidationFailed {
		t.Fatalf("expected CodeValidationFailed for an unknown mapping version, got %v", err)
	}
}

func TestValidateTieOut_UnaffectedByAggregation(t *testing.T) {
	balanced := []SnapshotContentAccount{
		acct("1000", "Cash", CategoryAsset, "300.25"),
		acct("1010", "Cash", CategoryAsset, "199.75"),
		acct("1900", "Suspense", CategoryAsset, "10"),
		acct("2900", "Suspense", CategoryLiability, "10"),
		acct("2000", "Payables", CategoryLiability, "100"),
		acct("2010", "Payables", CategoryLiability, "0.5"),
		acct("3000", "Equity", CategoryEquity, "399.5"),
		acct("4000", "Sales", CategoryRevenue, "1000"),
	}
	unbalanced := append(append([]SnapshotContentAccount{}, balanced...), acct("1020", "Cash", CategoryAsset, "0.01"))

	for name, accounts := range map[string][]SnapshotContentAccount{"balanced": balanced, "unbalanced": unbalanced} {
		v1, err := ValidateTieOut(mustBuild(t, MappingVersionV1, accounts...))
		if err != nil {
			t.Fatalf("%s V1: %v", name, err)
		}
		v2, err := ValidateTieOut(mustBuild(t, MappingVersionV2, accounts...))
		if err != nil {
			t.Fatalf("%s V2: %v", name, err)
		}
		if len(v1) != len(v2) || (len(v1) == 1 && v1[0] != v2[0]) {
			t.Fatalf("%s: aggregation changed the tie-out result: V1 %v, V2 %v", name, v1, v2)
		}
		if (name == "balanced") != (len(v2) == 0) {
			t.Fatalf("%s: unexpected tie-out result %v", name, v2)
		}
	}
}

func TestSources_FallsBackToTheFactItself(t *testing.T) {
	f := StatementFact{AccountCode: "1000", AccountName: "Cash", Category: CategoryAsset, Amount: "5"}
	s := f.Sources()
	if len(s) != 1 || s[0].AccountCode != "1000" || s[0].Balance != "5" {
		t.Fatalf("expected the fact itself as its only source, got %+v", s)
	}
}

// ── BuildComparative at V2 granularity ─────────────────────────────────────

func comparativeV2(t *testing.T, current, prior []SnapshotContentAccount) []ComparativeStatement {
	t.Helper()
	cur := mustBuild(t, MappingVersionV2, current...)
	pri := mustBuild(t, MappingVersionV2, prior...)
	for i := range cur {
		cur[i].FactID = "c-" + cur[i].AccountCode
	}
	for i := range pri {
		pri[i].FactID = "p-" + pri[i].AccountCode
	}
	got, err := BuildComparative(cur, pri, MappingVersionV2)
	if err != nil {
		t.Fatalf("BuildComparative: %v", err)
	}
	return got
}

func linesByName(stmts []ComparativeStatement) map[string][]ComparativeFact {
	out := map[string][]ComparativeFact{}
	for _, s := range stmts {
		for _, l := range s.Lines {
			out[l.AccountName] = append(out[l.AccountName], l)
		}
	}
	return out
}

func TestBuildComparativeV2_PairsAggregatedLinesByNameEvenWhenCompositionChanges(t *testing.T) {
	// Prior Cash = 1000 + 1010; current Cash = 1005 + 1010 (1000 closed,
	// 1005 opened). The representative code differs; it is still one line.
	got := comparativeV2(t,
		[]SnapshotContentAccount{
			acct("1005", "Cash", CategoryAsset, "70"), acct("1010", "Cash", CategoryAsset, "30"),
			acct("1500", "Inventory", CategoryAsset, "20"), // new line
		},
		[]SnapshotContentAccount{
			acct("1000", "Cash", CategoryAsset, "60"), acct("1010", "Cash", CategoryAsset, "15"),
			acct("2000", "Payables", CategoryLiability, "5"), // discontinued line
		},
	)
	lines := linesByName(got)
	cash := lines["Cash"]
	if len(cash) != 1 {
		t.Fatalf("expected one paired Cash line, got %+v", cash)
	}
	c := cash[0]
	if c.Change == nil || *c.Change != "25" || *c.CurrentAmount != "100" || *c.PriorAmount != "75" {
		t.Errorf("expected 100 vs 75, change 25; got %+v", c)
	}
	if len(c.CurrentSourceAccountCodes) != 2 || c.CurrentSourceAccountCodes[0] != "1005" ||
		len(c.PriorSourceAccountCodes) != 2 || c.PriorSourceAccountCodes[0] != "1000" {
		t.Errorf("expected each side's composition to be shown, got current %v prior %v", c.CurrentSourceAccountCodes, c.PriorSourceAccountCodes)
	}
	if inv := lines["Inventory"]; len(inv) != 1 || inv[0].PriorAmount != nil || inv[0].Change != nil {
		t.Errorf("Inventory (new): expected current only, no change; got %+v", inv)
	}
	if pay := lines["Payables"]; len(pay) != 1 || pay[0].CurrentAmount != nil || pay[0].Change != nil {
		t.Errorf("Payables (discontinued): expected prior only, no change; got %+v", pay)
	}
}

func TestBuildComparativeV2_ReclassificationStillVisible(t *testing.T) {
	got := comparativeV2(t,
		[]SnapshotContentAccount{acct("2900", "Suspense", CategoryLiability, "8")},
		[]SnapshotContentAccount{acct("1900", "Suspense", CategoryAsset, "5")},
	)
	s := linesByName(got)["Suspense"]
	if len(s) != 1 || s[0].Category != CategoryLiability || s[0].PriorCategory == nil || *s[0].PriorCategory != CategoryAsset || *s[0].Change != "3" {
		t.Fatalf("expected one reclassified Suspense line (LIABILITY, prior ASSET, change 3), got %+v", s)
	}
}

func TestBuildComparativeV2_SameNameInTwoCategories_PairsByCategory(t *testing.T) {
	// Current has ASSET and LIABILITY "Suspense"; prior only ASSET. The
	// ASSET lines pair; the LIABILITY line is new, not a reclassification.
	got := comparativeV2(t,
		[]SnapshotContentAccount{acct("1900", "Suspense", CategoryAsset, "9"), acct("2900", "Suspense", CategoryLiability, "4")},
		[]SnapshotContentAccount{acct("1900", "Suspense", CategoryAsset, "6")},
	)
	s := linesByName(got)["Suspense"]
	if len(s) != 2 {
		t.Fatalf("expected two Suspense lines, got %+v", s)
	}
	for _, l := range s {
		switch l.Category {
		case CategoryAsset:
			if l.Change == nil || *l.Change != "3" || l.PriorCategory != nil {
				t.Errorf("ASSET Suspense: expected paired with change 3, got %+v", l)
			}
		case CategoryLiability:
			if l.PriorAmount != nil || l.Change != nil {
				t.Errorf("LIABILITY Suspense: expected current only, got %+v", l)
			}
		}
	}
}

func TestBuildComparative_UnknownMappingVersionRefused(t *testing.T) {
	f := []StatementFact{{StatementType: StatementBalanceSheet, Category: CategoryAsset, AccountCode: "1", AccountName: "Cash", Amount: "1"}}
	if _, err := BuildComparative(f, f, "v9-made-up"); err == nil {
		t.Fatal("expected an unknown mapping version to be refused")
	}
}
