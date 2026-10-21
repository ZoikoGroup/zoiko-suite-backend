package domain

import "testing"

func fact(id string, stmt StatementType, cat AccountCategory, code, amount string) StatementFact {
	return StatementFact{FactID: id, StatementType: stmt, Category: cat, AccountCode: code, AccountName: "acct " + code, Amount: amount}
}

func TestBuildComparative_PairsOverlappingAndKeepsOneSidedLines(t *testing.T) {
	current := []StatementFact{
		fact("c1", StatementBalanceSheet, CategoryAsset, "1000", "750.25"),
		fact("c2", StatementBalanceSheet, CategoryAsset, "1100", "50"), // new this period
		fact("c3", StatementIncomeStatement, CategoryRevenue, "4000", "900"),
	}
	prior := []StatementFact{
		fact("p1", StatementBalanceSheet, CategoryAsset, "1000", "500.00"),
		fact("p2", StatementBalanceSheet, CategoryLiability, "2000", "100"), // discontinued
		fact("p3", StatementIncomeStatement, CategoryRevenue, "4000", "1000.10"),
	}
	got, err := BuildComparative(current, prior, MappingVersionV1)
	if err != nil {
		t.Fatalf("BuildComparative: %v", err)
	}
	if len(got) != 2 || got[0].StatementType != StatementBalanceSheet || got[1].StatementType != StatementIncomeStatement {
		t.Fatalf("expected BALANCE_SHEET then INCOME_STATEMENT, got %+v", got)
	}

	bs := got[0].Lines
	if len(bs) != 3 || bs[0].AccountCode != "1000" || bs[1].AccountCode != "1100" || bs[2].AccountCode != "2000" {
		t.Fatalf("expected balance sheet lines 1000, 1100, 2000 in order, got %+v", bs)
	}
	if bs[0].Change == nil || *bs[0].Change != "250.25" {
		t.Errorf("1000: expected change 250.25, got %v", bs[0].Change)
	}
	if bs[1].PriorAmount != nil || bs[1].Change != nil || bs[1].CurrentAmount == nil || *bs[1].CurrentAmount != "50" {
		t.Errorf("1100 (new): expected current only and no change, got %+v", bs[1])
	}
	if bs[2].CurrentAmount != nil || bs[2].Change != nil || bs[2].PriorAmount == nil || *bs[2].PriorFactID != "p2" {
		t.Errorf("2000 (discontinued): expected prior only and no change, got %+v", bs[2])
	}

	is := got[1].Lines
	if len(is) != 1 || is[0].Change == nil || *is[0].Change != "-100.1" {
		t.Errorf("4000: expected exact-decimal change -100.1, got %+v", is)
	}
}

func TestBuildComparative_ReclassificationIsVisible(t *testing.T) {
	// Same statement, different category: one paired line carrying both.
	got, err := BuildComparative(
		[]StatementFact{fact("c1", StatementBalanceSheet, CategoryLiability, "2500", "10")},
		[]StatementFact{fact("p1", StatementBalanceSheet, CategoryAsset, "2500", "10")},
		MappingVersionV1,
	)
	if err != nil {
		t.Fatalf("BuildComparative: %v", err)
	}
	l := got[0].Lines[0]
	if l.Category != CategoryLiability || l.PriorCategory == nil || *l.PriorCategory != CategoryAsset {
		t.Errorf("expected category LIABILITY with prior_category ASSET, got %+v", l)
	}

	// Different statements: one line on each, neither dropped.
	got, err = BuildComparative(
		[]StatementFact{fact("c1", StatementIncomeStatement, CategoryExpense, "5000", "10")},
		[]StatementFact{fact("p1", StatementBalanceSheet, CategoryAsset, "5000", "10")},
		MappingVersionV1,
	)
	if err != nil {
		t.Fatalf("BuildComparative: %v", err)
	}
	if len(got) != 2 || len(got[0].Lines) != 1 || len(got[1].Lines) != 1 {
		t.Fatalf("expected the account on both statements, got %+v", got)
	}
}

func TestBuildComparative_FailsClosedOnBadInput(t *testing.T) {
	dup := []StatementFact{
		fact("a", StatementBalanceSheet, CategoryAsset, "1000", "1"),
		fact("b", StatementBalanceSheet, CategoryAsset, "1000", "2"),
	}
	if _, err := BuildComparative(dup, nil, MappingVersionV1); err == nil {
		t.Error("expected a duplicate account code in one period to be refused")
	}
	notNum := []StatementFact{fact("a", StatementBalanceSheet, CategoryAsset, "1000", "abc")}
	if _, err := BuildComparative(notNum, nil, MappingVersionV1); err == nil {
		t.Error("expected an unparseable amount to be refused")
	}
}
