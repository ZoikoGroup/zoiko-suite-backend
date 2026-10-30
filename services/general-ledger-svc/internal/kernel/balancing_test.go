package kernel

import (
	"math/big"
	"strings"
	"testing"
)

func balancingDir() fakeDir {
	d := dir()
	d.accounts["map:DTO"], d.accounts["map:DFR"] = acct("2900"), acct("1900")
	return d
}

// units puts each line of the invoice rule in a unit: AR in a, revenue in b, tax in c.
func balancedBy(a, b, c string, cfg *BalancingDimension) *RuleSet {
	content := invoiceContent()
	content.LineTemplates[0].Dimensions = map[string]string{"unit": a}
	content.LineTemplates[1].Dimensions = map[string]string{"unit": b}
	content.LineTemplates[2].Dimensions = map[string]string{"unit": c}
	content.BalancingDimension = cfg
	return rule(content)
}

var dueAccounts = &BalancingDimension{Dimension: "unit", DueTo: &AccountRef{MappingKey: "DTO"}, DueFrom: &AccountRef{MappingKey: "DFR"}}

func unitNet(p *Proposal, unit string) (debit, credit string) {
	d, c := new(big.Rat), new(big.Rat)
	for _, l := range p.Lines {
		if l.Dimensions["unit"] != unit {
			continue
		}
		if l.Side == Debit {
			d.Add(d, dec(l.AmountFunctional))
		} else {
			c.Add(c, dec(l.AmountFunctional))
		}
	}
	return ratFixed(d), ratFixed(c)
}

func TestBalancingDimensionLeavesABalancedJournalAlone(t *testing.T) {
	// AR 120 debit; revenue 100 and tax 20 credit — all in one unit, so each unit balances by itself
	p, err := eval(t, event(okFacts()), balancedBy("A", "A", "A", dueAccounts), balancingDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 3 {
		t.Fatalf("a journal that already balances by the dimension gains no lines: %d", len(p.Lines))
	}
}

func TestBalancingDimensionAddsExplicitDueToAndDueFromLines(t *testing.T) {
	// AR 120 in unit A, revenue 100 and tax 20 in unit B: A is 120 out one way, B the other
	p, err := eval(t, event(okFacts()), balancedBy("A", "B", "B", dueAccounts), balancingDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 5 {
		t.Fatalf("two explicit lines are added: %+v", p.Lines)
	}
	byRef := map[string]ProposalLine{}
	for _, l := range p.Lines {
		byRef[l.LineRef] = l
	}
	// A has debited more than it credited: it owes the other unit, so it credits the due-to account
	if l := byRef["DUE_TO"]; l.Side != Credit || l.AccountCode != "2900" || l.AmountFunctional != "120.00" || l.Dimensions["unit"] != "A" || l.AmountTx != "120.00" {
		t.Fatalf("%+v", l)
	}
	if l := byRef["DUE_FROM"]; l.Side != Debit || l.AccountCode != "1900" || l.AmountFunctional != "120.00" || l.Dimensions["unit"] != "B" {
		t.Fatalf("%+v", l)
	}
	// every unit now balances, and so does the journal as a whole
	for _, u := range []string{"A", "B"} {
		if d, c := unitNet(p, u); d != c {
			t.Fatalf("unit %s: debit %s credit %s", u, d, c)
		}
	}
	if p.TotalDebitFunc != p.TotalCreditFunc || p.TotalDebitTx != p.TotalCreditTx || p.TotalDebitFunc != "240.00" {
		t.Fatalf("%s %s", p.TotalDebitFunc, p.TotalCreditFunc)
	}
	said := false
	for _, s := range p.Trace {
		said = said || (s.Step == "balancing" && strings.Contains(s.Detail, "unit=A DUE_TO") && strings.Contains(s.Detail, "unit=B DUE_FROM"))
	}
	if !said {
		t.Fatalf("the evidence says what was added: %+v", p.Trace)
	}
	// deterministic: the same event gives the same proposal
	p2, _ := eval(t, event(okFacts()), balancedBy("A", "B", "B", dueAccounts), balancingDir(), nil, nil)
	if p2.ProposalHash != p.ProposalHash {
		t.Fatal("the same inputs, the same proposal")
	}
}

func TestBalancingDimensionTreatsLinesWithNoValueAsTheirOwnGroup(t *testing.T) {
	// the tax line carries no unit: the lines with no unit are a group of their own and are balanced like any other
	p, err := eval(t, event(okFacts()), balancedBy("A", "A", "", dueAccounts), balancingDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, l := range p.Lines {
		if l.LineRef == "DUE_FROM" && l.Dimensions["unit"] == "" && l.AmountFunctional == "20.00" {
			seen = true
		}
	}
	if !seen || len(p.Lines) != 5 {
		t.Fatalf("%+v", p.Lines)
	}
}

func TestBalancingDimensionWithoutAccountsRefusesRatherThanAbsorbs(t *testing.T) {
	_, err := eval(t, event(okFacts()), balancedBy("A", "B", "B", &BalancingDimension{Dimension: "unit"}), balancingDir(), nil, nil)
	wantFam(t, err, FamBal, "DIMENSION_UNBALANCED")
	// unconfigured, the engine behaves as it always did
	if _, err := eval(t, event(okFacts()), balancedBy("A", "B", "B", nil), balancingDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBalancingAccountThatCannotBePostedToIsRefused(t *testing.T) {
	d := balancingDir()
	d.accounts["map:DTO"] = &AccountInfo{Code: "2900", Postable: true, Active: false}
	_, err := eval(t, event(okFacts()), balancedBy("A", "B", "B", dueAccounts), d, nil, nil)
	wantFam(t, err, FamCOA, "BALANCING_ACCOUNT_UNAVAILABLE")
}

func TestBalancingInForeignCurrencyCarriesBothAmounts(t *testing.T) {
	ev := event(okFacts())
	ev.TransactionCurrency = "USD"
	ev.PayloadHash = ev.ComputeHash()
	fx := &FXBasis{Rate: "0.8", RateID: "r1", Source: "ecb", RateType: "SPOT", AsOf: "2026-02-01"}
	p, err := eval(t, ev, balancedBy("A", "B", "B", dueAccounts), balancingDir(), fx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range p.Lines {
		if l.LineRef == "DUE_TO" && (l.AmountTx != "120.00" || l.AmountFunctional != "96.00" || l.Currency != "USD") {
			t.Fatalf("the carried difference has its transaction and its functional amount: %+v", l)
		}
	}
	for _, u := range []string{"A", "B"} {
		if d, c := unitNet(p, u); d != c {
			t.Fatalf("unit %s: %s %s", u, d, c)
		}
	}
}

func TestBalancingDimensionValidation(t *testing.T) {
	for name, cfg := range map[string]*BalancingDimension{
		"no dimension":   {Dimension: "", DueTo: dueAccounts.DueTo, DueFrom: dueAccounts.DueFrom},
		"bad dimension":  {Dimension: "not a code"},
		"only due-to":    {Dimension: "unit", DueTo: dueAccounts.DueTo},
		"only due-from":  {Dimension: "unit", DueFrom: dueAccounts.DueFrom},
		"both ref forms": {Dimension: "unit", DueTo: &AccountRef{AccountCode: "1", MappingKey: "X"}, DueFrom: dueAccounts.DueFrom},
	} {
		if err := balancedBy("A", "B", "B", cfg).Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if err := balancedBy("A", "B", "B", dueAccounts).Validate(); err != nil {
		t.Fatal(err)
	}
	if balancedBy("A", "B", "B", dueAccounts).ContentHash == balancedBy("A", "B", "B", nil).ContentHash {
		t.Fatal("the content hash must cover the balancing dimension")
	}
}
