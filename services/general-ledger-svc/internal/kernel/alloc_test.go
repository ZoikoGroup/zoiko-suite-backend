package kernel

import (
	"math/big"
	"testing"
)

func TestDistributeAlwaysAccountsForTheWholePool(t *testing.T) {
	pool := big.NewRat(10000, 100) // 100.00 over three equal drivers: 33.33 x 3 leaves 0.01
	amounts, exact, residual := Distribute(pool, []*big.Rat{big.NewRat(1, 1), big.NewRat(1, 1), big.NewRat(1, 1)}, 2)
	sum := new(big.Rat)
	for _, a := range amounts {
		sum.Add(sum, a)
	}
	if amounts[0].FloatString(2) != "33.33" || residual.FloatString(2) != "0.01" || new(big.Rat).Add(sum, residual).Cmp(pool) != 0 {
		t.Fatalf("%v residual %s", amounts, residual.FloatString(2))
	}
	if exact[0].FloatString(4) != "33.3333" {
		t.Fatalf("the exact share is kept for the record: %s", exact[0].FloatString(4))
	}
}

func TestDistributeByUnequalDrivers(t *testing.T) {
	amounts, _, residual := Distribute(big.NewRat(1000, 1), []*big.Rat{big.NewRat(3, 1), big.NewRat(1, 1)}, 2)
	if amounts[0].FloatString(2) != "750.00" || amounts[1].FloatString(2) != "250.00" || residual.Sign() != 0 {
		t.Fatalf("%v %v", amounts, residual)
	}
	// a recipient with a zero driver gets nothing
	amounts, _, _ = Distribute(big.NewRat(10, 1), []*big.Rat{big.NewRat(0, 1), big.NewRat(2, 1)}, 2)
	if amounts[0].Sign() != 0 || amounts[1].FloatString(2) != "10.00" {
		t.Fatalf("%v", amounts)
	}
}

func fixed(pcts ...string) AllocContent {
	c := AllocContent{Name: "n", SourceAccounts: []string{"6000"}, Driver: AllocFixed}
	for i, p := range pcts {
		c.Recipients = append(c.Recipients, AllocRecipient{Key: string(rune('A' + i)), Account: "61" + string(rune('0'+i)) + "0", Percent: p})
	}
	return c
}

func TestAllocationRuleContentIsValidated(t *testing.T) {
	good := fixed("60", "40")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]AllocContent{
		"percentages must add to 100":        fixed("60", "30"),
		"one recipient is not an allocation": fixed("100"),
		"a recipient may not be the source": func() AllocContent {
			c := fixed("50", "50")
			c.Recipients[0].Account = "6000"
			return c
		}(),
		"duplicate recipient keys": func() AllocContent {
			c := fixed("50", "50")
			c.Recipients[1].Key = c.Recipients[0].Key
			return c
		}(),
		"a named driver takes no percentages": func() AllocContent {
			c := fixed("50", "50")
			c.Driver = "headcount"
			return c
		}(),
		"no source": func() AllocContent {
			c := fixed("50", "50")
			c.SourceAccounts = nil
			return c
		}(),
		"zero percent": fixed("100", "0"),
	}
	for what, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: accepted", what)
		}
	}
}

func TestJurisdictionExtensionOverridesTheGlobalRuleOnlyInItsBooks(t *testing.T) {
	global := RuleSet{Name: "global"}
	de := RuleSet{Name: "de", Jurisdiction: "DE"}
	if got := preferJurisdiction([]RuleSet{global, de}, "DE"); len(got) != 1 || got[0].Name != "de" {
		t.Fatalf("%v", got)
	}
	if got := preferJurisdiction([]RuleSet{global}, "DE"); len(got) != 1 || got[0].Name != "global" {
		t.Fatalf("a book with no extension for its jurisdiction keeps the global rule: %v", got)
	}
	if got := preferJurisdiction([]RuleSet{global}, ""); len(got) != 1 {
		t.Fatal("a global book uses the global rule")
	}
	if !global.AppliesToJurisdiction("FR") || !de.AppliesToJurisdiction("DE") || de.AppliesToJurisdiction("FR") || de.AppliesToJurisdiction("") {
		t.Fatal("an extension applies only to books of its jurisdiction")
	}
	for _, ok := range []string{"", "DE", "US-CA"} {
		if !jurisdictionRE.MatchString(ok) {
			t.Fatalf("%q is a valid jurisdiction", ok)
		}
	}
	for _, bad := range []string{"de", "GERMANY", "D", "US-"} {
		if jurisdictionRE.MatchString(bad) {
			t.Fatalf("%q is not a jurisdiction", bad)
		}
	}
}

func TestCompareBooksShowsWhereTheBooksDiffer(t *testing.T) {
	line := func(acct, d, c string) TraceLine { return TraceLine{AccountCode: acct, Debit: d, Credit: c} }
	tr := &Trace{Event: &AccountingEvent{AccountingEventID: "ev"}, Postings: []TracePosting{
		{Book: &TraceBook{BookCode: "STAT", BookType: "PRIMARY_STATUTORY"}, Journals: []TraceJournal{{Role: "POSTING", JournalNumber: "J1", Status: "FINALIZED",
			Lines: []TraceLine{line("1100", "120.00", "0"), line("4000", "0", "100.00"), line("2200", "0", "20.00")}}}},
		{Book: &TraceBook{BookCode: "MGMT", BookType: "MANAGEMENT"}, Journals: []TraceJournal{{Role: "POSTING", JournalNumber: "J2", Status: "FINALIZED",
			Lines: []TraceLine{line("1100", "120.00", "0"), line("4100", "0", "120.00")}}}},
	}}
	c := CompareBooks(tr)
	if !c.SingleSource || len(c.Books) != 2 || c.Books[0].BookCode != "MGMT" {
		t.Fatalf("%+v", c)
	}
	diff := map[string]BookDifference{}
	for _, d := range c.Differences {
		diff[d.AccountCode] = d
	}
	if _, same := diff["1100"]; same {
		t.Fatal("the receivable is the same in both books")
	}
	if diff["4000"].ByBook["STAT"] != "-100.00" || diff["4000"].ByBook["MGMT"] != "0.00" || diff["4100"].ByBook["MGMT"] != "-120.00" || len(c.Differences) != 3 {
		t.Fatalf("%+v", c.Differences)
	}
}
