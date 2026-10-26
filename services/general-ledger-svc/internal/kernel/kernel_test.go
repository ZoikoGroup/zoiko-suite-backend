package kernel

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

const (
	tenant = "11111111-1111-4111-8111-111111111111"
	entity = "22222222-2222-4222-8222-222222222222"
)

var d0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeDir struct {
	accounts map[string]*AccountInfo
	units    map[string]int
	badDims  map[string]bool
	down     bool
}

func (f fakeDir) MinorUnits(_ context.Context, c string, _ time.Time) (int, error) {
	if f.down {
		return 0, errors.New("registry down")
	}
	if u, ok := f.units[c]; ok {
		return u, nil
	}
	return 2, nil
}

func (f fakeDir) ResolveAccount(_ context.Context, ref AccountRef, _ string, _ time.Time) (*AccountInfo, error) {
	key := ref.AccountCode
	if ref.MappingKey != "" {
		key = "map:" + ref.MappingKey
	}
	a, ok := f.accounts[key]
	if !ok {
		return nil, errors.New("no such account")
	}
	return a, nil
}

func (f fakeDir) DimensionAllowed(_ context.Context, dim, val, _, _ string, _ time.Time) (bool, error) {
	return !f.badDims[dim+"="+val], nil
}

func acct(code string) *AccountInfo { return &AccountInfo{Code: code, Postable: true, Active: true} }

func dir() fakeDir {
	return fakeDir{accounts: map[string]*AccountInfo{"map:AR": acct("1100"), "map:REV": acct("4000"), "map:VAT": acct("2200"), "map:ROUND": acct("7999"),
		"1100": acct("1100"), "4000": acct("4000"), "2200": acct("2200")}}
}

func book() *Book {
	return &Book{BookID: "b1", TenantID: tenant, LegalEntityID: entity, BookCode: "STAT", BookType: BookStatutory, AccountingFrameworkID: "IFRS", FunctionalCurrency: "GBP",
		FiscalCalendarID: "CAL", ChartOfAccountsID: "COA", DefaultLedgerID: "LED", EffectiveFrom: d0, Status: BookActive}
}

func invoiceContent() RuleContent {
	return RuleContent{
		Conditions:       []Condition{{Fact: "document_type", Op: "eq", Value: "INVOICE"}},
		RoundingPolicyID: "HALF_EVEN",
		LineTemplates: []LineTemplate{
			{LineRef: "L1", Side: Debit, Account: AccountRef{MappingKey: "AR"}, Amount: "gross"},
			{LineRef: "L2", Side: Credit, Account: AccountRef{MappingKey: "REV"}, Amount: "net"},
			{LineRef: "L3", Side: Credit, Account: AccountRef{MappingKey: "VAT"}, Amount: "tax", When: []Condition{{Fact: "tax", Op: "gt", Value: "0"}}},
		},
	}
}

func rule(c RuleContent) *RuleSet {
	rs := &RuleSet{RuleSetID: "r1", TenantID: tenant, Name: "ar-invoice", Domain: "AR", EventType: "AR.INVOICE_ISSUED", BookType: BookStatutory, FrameworkID: "IFRS",
		Version: "1.0.0", Status: RuleActive, EffectiveFrom: d0, Content: c, TestPackID: "tp-1",
		TestPack: []TestCase{{Name: "basic", Event: TestEvent{EventType: "AR.INVOICE_ISSUED", TransactionCurrency: "GBP",
			Facts: map[string]string{"document_type": "INVOICE", "gross": "120.00", "net": "100.00", "tax": "20.00"},
		}, Accounts: map[string]string{"AR": "1100", "REV": "4000", "VAT": "2200", "ROUND": "7999"},
			Expect: TestExpectation{Lines: []ExpectedLine{{Side: Debit, AccountCode: "1100", Amount: "120.00"}, {Side: Credit, AccountCode: "4000", Amount: "100.00"}, {Side: Credit, AccountCode: "2200", Amount: "20.00"}}}}}}
	rs.ContentHash = HashContent(rs.Content, rs.TestPack)
	return rs
}

func event(facts map[string]string) *AccountingEvent {
	e := &AccountingEvent{AccountingEventID: "e1", TenantID: tenant, EventType: "AR.INVOICE_ISSUED", EventVersion: 1, LegalEntityID: entity, SourceSystem: "ar",
		SourceObjectType: "invoice", SourceObjectID: "inv-1", SourceEventID: "se-1", OccurredAt: d0, TransactionCurrency: "GBP", Facts: facts,
		IdempotencyKey: "k", CorrelationID: "c", CausationID: "ca"}
	e.PayloadHash = e.ComputeHash()
	return e
}

func okFacts() map[string]string {
	return map[string]string{"document_type": "INVOICE", "gross": "120.00", "net": "100.00", "tax": "20.00"}
}

func eval(t *testing.T, ev *AccountingEvent, rs *RuleSet, d Directory, fx *FXBasis, bk *Book) (*Proposal, error) {
	t.Helper()
	if bk == nil {
		bk = book()
	}
	return Evaluate(context.Background(), EvalInput{Event: ev, Book: bk, Rule: rs, PostingDate: d0.AddDate(0, 1, 0), FX: fx, Directory: d})
}

func wantFam(t *testing.T, err error, fam, code string) {
	t.Helper()
	e, ok := AsException(err)
	if !ok || e.Family != fam || (code != "" && e.Code != code) {
		t.Fatalf("want %s/%s, got %v", fam, code, err)
	}
}

func TestRoundingModes(t *testing.T) {
	cases := []struct {
		in, mode string
		scale    int
		want     string
	}{
		{"2.345", HalfEven, 2, "2.34"}, {"2.355", HalfEven, 2, "2.36"}, {"2.345", HalfUp, 2, "2.35"}, {"-2.345", HalfUp, 2, "-2.35"},
		{"-2.345", HalfEven, 2, "-2.34"}, {"2.349", Down, 2, "2.34"}, {"2.341", Up, 2, "2.35"}, {"0.5", HalfEven, 0, "0"}, {"1.5", HalfEven, 0, "2"},
		{"100", HalfEven, 2, "100.00"}, {"0.0049", HalfUp, 2, "0.00"}, {"7.0005", HalfUp, 3, "7.001"},
	}
	for _, c := range cases {
		r, ok := ParseDecimal(c.in)
		if !ok {
			t.Fatal(c.in)
		}
		if got := FormatMinor(RoundMinor(r, c.scale, c.mode), c.scale); got != c.want {
			t.Errorf("%s %s scale %d = %s, want %s", c.in, c.mode, c.scale, got, c.want)
		}
	}
	for _, bad := range []string{"1e3", "1/3", "1,000", "", "1.", ".5", "--1", "0x10", "1.0000000000000000001"} {
		if _, ok := ParseDecimal(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestExpressions(t *testing.T) {
	env := map[string]*big.Rat{"gross": big.NewRat(120, 1), "rate": big.NewRat(1, 5), "n": big.NewRat(3, 1)}
	for src, want := range map[string]string{"gross": "120", "gross * rate": "24", "gross - gross * rate": "96", "(gross + 30) / n": "50", "-gross + 200": "80", "2 * 3 + 4": "10", "gross/n/4": "10"} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		v, err := e.Eval(env)
		if err != nil || ratString(v) != want {
			t.Errorf("%s = %v (%v), want %s", src, v, err, want)
		}
	}
	for _, src := range []string{"", "gross +", "(gross", "gross )", "gross $ 2", "1..2", "Gross", "a.b.c", strings.Repeat("(", 50) + "1" + strings.Repeat(")", 50), "foo()"} {
		if _, err := ParseExpr(src); err == nil {
			t.Errorf("%q must not parse", src)
		}
	}
	e, _ := ParseExpr("gross / (n - 3)")
	_, err := e.Eval(env)
	wantFam(t, err, FamData, "DIVISION_BY_ZERO")
	e, _ = ParseExpr("missing * 2")
	_, err = e.Eval(env)
	wantFam(t, err, FamRule, "FACT_MISSING")
	if e2, _ := ParseExpr("gross + net + gross"); strings.Join(e2.Refs(), ",") != "gross,net" {
		t.Errorf("refs %v", e2.Refs())
	}
}

func TestEvaluateBalancedAndDeterministic(t *testing.T) {
	p, err := eval(t, event(okFacts()), rule(invoiceContent()), dir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 3 || p.TotalDebitTx != "120.00" || p.TotalCreditTx != "120.00" || p.TotalDebitFunc != p.TotalCreditFunc {
		t.Fatalf("%+v", p)
	}
	if p.Provenance.RuleName != "ar-invoice" || p.Provenance.RuleVersion != "1.0.0" || p.Provenance.ContentHash == "" || len(p.Provenance.Branch) != 3 {
		t.Fatalf("provenance %+v", p.Provenance)
	}
	again, _ := eval(t, event(okFacts()), rule(invoiceContent()), dir(), nil, nil)
	if again.ProposalHash != p.ProposalHash || p.ProposalHash == "" {
		t.Fatal("the same input must give the same proposal, byte for byte")
	}
	f := okFacts()
	f["net"] = "99.00"
	f["gross"] = "119.00"
	other, _ := eval(t, event(f), rule(invoiceContent()), dir(), nil, nil)
	if other.ProposalHash == p.ProposalHash {
		t.Fatal("a different event must give a different proposal hash")
	}
	// a zero-tax invoice skips the tax line through its own condition
	f = okFacts()
	f["tax"], f["gross"] = "0.00", "100.00"
	p0, err := eval(t, event(f), rule(invoiceContent()), dir(), nil, nil)
	if err != nil || len(p0.Lines) != 2 {
		t.Fatalf("%v %+v", err, p0)
	}
}

func TestRuleSelectionRefusals(t *testing.T) {
	f := okFacts()
	f["document_type"] = "CREDIT_NOTE"
	_, err := eval(t, event(f), rule(invoiceContent()), dir(), nil, nil)
	wantFam(t, err, FamRule, "NO_MATCHING_BRANCH")
	rs := rule(invoiceContent())
	rs.Status = RuleApproved
	_, err = eval(t, event(okFacts()), rs, dir(), nil, nil)
	wantFam(t, err, FamRule, "RULE_NOT_IN_FORCE")
	rs = rule(invoiceContent())
	rs.BookType = BookManagement
	_, err = eval(t, event(okFacts()), rs, dir(), nil, nil)
	wantFam(t, err, FamRule, "RULE_NOT_APPLICABLE")
	bk := book()
	bk.Status = BookSuspended
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), dir(), nil, bk)
	wantFam(t, err, FamRule, "BOOK_NOT_IN_FORCE")
	bk = book()
	bk.LegalEntityID = "33333333-3333-4333-8333-333333333333"
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), dir(), nil, bk)
	wantFam(t, err, FamAuth, "ENTITY_MISMATCH")
	f = okFacts()
	delete(f, "net")
	_, err = eval(t, event(f), rule(invoiceContent()), dir(), nil, nil)
	wantFam(t, err, FamRule, "FACT_MISSING")
}

func TestUnbalancedAndResidual(t *testing.T) {
	// three equal shares of 10.00 each round to 3.33: the credits are one cent short
	c := RuleContent{RoundingPolicyID: "HALF_EVEN", LineTemplates: []LineTemplate{
		{LineRef: "D", Side: Debit, Account: AccountRef{MappingKey: "AR"}, Amount: "gross"},
		{LineRef: "C1", Side: Credit, Account: AccountRef{MappingKey: "REV"}, Amount: "gross / 3"},
		{LineRef: "C2", Side: Credit, Account: AccountRef{MappingKey: "REV"}, Amount: "gross / 3"},
		{LineRef: "C3", Side: Credit, Account: AccountRef{MappingKey: "VAT"}, Amount: "gross / 3"},
	}}
	f := map[string]string{"gross": "10.00"}
	_, err := eval(t, event(f), rule(c), dir(), nil, nil)
	wantFam(t, err, FamBal, "UNBALANCED")
	c.RoundingPolicyID = "HALF_EVEN_RESIDUAL"
	c.RoundingAccount = &AccountRef{MappingKey: "ROUND"}
	p, err := eval(t, event(f), rule(c), dir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := p.Lines[len(p.Lines)-1]
	if len(p.Lines) != 5 || !last.Rounding || last.Side != Credit || last.AmountTx != "0.01" || last.AccountCode != "7999" || p.TotalDebitTx != p.TotalCreditTx {
		t.Fatalf("an explicit rounding line is required, not a silent adjustment: %+v", p.Lines)
	}
	// a residual larger than the tolerance is still refused
	c.LineTemplates[1].Amount = "gross / 3 - 0.10"
	_, err = eval(t, event(f), rule(c), dir(), nil, nil)
	wantFam(t, err, FamBal, "UNBALANCED")
	// a negative amount is the rule's mistake, never an implicit side flip
	c.LineTemplates[1].Amount = "0 - gross"
	_, err = eval(t, event(f), rule(c), dir(), nil, nil)
	wantFam(t, err, FamData, "NEGATIVE_AMOUNT")
}

func TestForeignCurrency(t *testing.T) {
	f := okFacts()
	ev := event(f)
	ev.TransactionCurrency = "USD"
	ev.PayloadHash = ev.ComputeHash()
	_, err := eval(t, ev, rule(invoiceContent()), dir(), nil, nil)
	wantFam(t, err, FamFX, "RATE_MISSING")
	for _, bad := range []*FXBasis{{Rate: "0", RateType: "SPOT", Source: "ECB", AsOf: "2026-01-31"}, {Rate: "abc", RateType: "SPOT", Source: "ECB", AsOf: "2026-01-31"},
		{Rate: "0.8", RateType: "", Source: "ECB", AsOf: "2026-01-31"}} {
		_, err = eval(t, ev, rule(invoiceContent()), dir(), bad, nil)
		if e, ok := AsException(err); !ok || e.Family != FamFX {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	c := invoiceContent()
	c.RoundingPolicyID = "HALF_EVEN_RESIDUAL"
	c.RoundingAccount = &AccountRef{MappingKey: "ROUND"}
	// 0.7333 turns 100.00/20.00/120.00 into 73.33/14.67/88.00: 73.33+14.67 = 88.00 balances exactly
	p, err := eval(t, ev, rule(c), dir(), &FXBasis{Rate: "0.7333", RateType: "SPOT", Source: "ECB", AsOf: "2026-01-31", RateID: "fx-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.FunctionalCcy != "GBP" || p.TransactionCcy != "USD" || p.FX == nil || p.FX.RateID != "fx-1" || p.TotalDebitFunc != p.TotalCreditFunc {
		t.Fatalf("%+v", p)
	}
	for _, l := range p.Lines {
		if l.AmountTx == "" || l.AmountFunctional == "" || l.Currency != "USD" {
			t.Fatalf("original and functional amounts are both kept: %+v", l)
		}
	}
	// half-cent ties round to even on each line, so the converted credits can differ from the debit by a cent
	tie := event(map[string]string{"document_type": "INVOICE", "gross": "120.02", "net": "100.01", "tax": "20.01"})
	tie.TransactionCurrency = "USD"
	tie.PayloadHash = tie.ComputeHash()
	half := &FXBasis{Rate: "0.5", RateType: "SPOT", Source: "ECB", AsOf: "2026-01-31"}
	p, err = eval(t, tie, rule(c), dir(), half, nil)
	if err != nil || p.TotalDebitFunc != p.TotalCreditFunc || !p.Lines[len(p.Lines)-1].Rounding {
		t.Fatalf("the cent is closed by an explicit rounding line: %v %+v", err, p)
	}
	// without a residual policy the same conversion is refused
	_, err = eval(t, tie, rule(invoiceContent()), dir(), half, nil)
	wantFam(t, err, FamBal, "UNBALANCED")
	// a zero-decimal functional currency
	bk := book()
	bk.FunctionalCurrency = "JPY"
	d := dir()
	d.units = map[string]int{"JPY": 0}
	p, err = eval(t, ev, rule(c), d, &FXBasis{Rate: "150.5", RateType: "SPOT", Source: "ECB", AsOf: "2026-01-31"}, bk)
	if err != nil || strings.Contains(p.TotalDebitFunc, ".") {
		t.Fatalf("a JPY total has no decimals: %v %+v", err, p)
	}
	d.down = true
	_, err = eval(t, ev, rule(c), d, nil, nil)
	wantFam(t, err, FamExt, "CURRENCY_UNAVAILABLE")
}

func TestTaxDependency(t *testing.T) {
	c := invoiceContent()
	c.TaxDependency = &TaxDependency{RequiredFacts: []string{"tax_result_id"}}
	_, err := eval(t, event(okFacts()), rule(c), dir(), nil, nil)
	wantFam(t, err, FamTax, "TAX_RESULT_MISSING")
	f := okFacts()
	f["tax_result_id"] = "tx-1"
	if _, err := eval(t, event(f), rule(c), dir(), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccountAndDimensionControls(t *testing.T) {
	d := dir()
	d.accounts["map:REV"] = &AccountInfo{Code: "4000", Postable: false, Active: true}
	_, err := eval(t, event(okFacts()), rule(invoiceContent()), d, nil, nil)
	wantFam(t, err, FamCOA, "ACCOUNT_NOT_POSTABLE")
	d = dir()
	d.accounts["map:REV"] = &AccountInfo{Code: "4000", Postable: true, Active: false}
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), d, nil, nil)
	wantFam(t, err, FamCOA, "ACCOUNT_NOT_POSTABLE")
	d = dir()
	d.accounts["map:REV"] = &AccountInfo{Code: "4000", Postable: true, Active: true, CurrencyRestricted: []string{"EUR"}}
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), d, nil, nil)
	wantFam(t, err, FamCOA, "ACCOUNT_CURRENCY_RESTRICTED")
	delete(d.accounts, "map:REV")
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), d, nil, nil)
	wantFam(t, err, FamCOA, "ACCOUNT_UNRESOLVED")

	c := invoiceContent()
	c.DimensionRules = []DimensionRule{{Dimension: "cost_center", Mode: DimRequired}}
	_, err = eval(t, event(okFacts()), rule(c), dir(), nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_MISSING")
	ev := event(okFacts())
	ev.Dimensions = map[string]string{"cost_center": "CC1"}
	p, err := eval(t, ev, rule(c), dir(), nil, nil)
	if err != nil || p.Lines[0].Dimensions["cost_center"] != "CC1" {
		t.Fatalf("%v %+v", err, p)
	}
	bd := dir()
	bd.badDims = map[string]bool{"cost_center=CC1": true}
	_, err = eval(t, ev, rule(c), bd, nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_VALUE_INVALID")
	c.DimensionRules = []DimensionRule{{Dimension: "cost_center", Mode: DimProhibited}}
	_, err = eval(t, ev, rule(c), dir(), nil, nil)
	if err == nil {
		// an event-level dimension that the rule does not use is not carried onto the line
	}
	c.LineTemplates[0].Dimensions = map[string]string{"cost_center": "$dimensions.cost_center"}
	_, err = eval(t, ev, rule(c), dir(), nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_PROHIBITED")
	// derived from a fact, visible on the line
	c = invoiceContent()
	c.DimensionRules = []DimensionRule{{Dimension: "project", Mode: DimDerived, From: "$facts.project_code"}}
	f := okFacts()
	f["project_code"] = "P-9"
	p, err = eval(t, event(f), rule(c), dir(), nil, nil)
	if err != nil || p.Lines[1].Dimensions["project"] != "P-9" {
		t.Fatalf("derived dimensions stay visible in the evidence: %v %+v", err, p)
	}
	// an account that requires a dimension the rule does not supply
	d = dir()
	d.accounts["map:REV"] = &AccountInfo{Code: "4000", Postable: true, Active: true, DimensionPolicy: map[string]string{"department": DimRequired}}
	_, err = eval(t, event(okFacts()), rule(invoiceContent()), d, nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_MISSING")
}

func TestRuleSetValidation(t *testing.T) {
	if err := rule(invoiceContent()).Validate(); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(r *RuleSet)) error { r := rule(invoiceContent()); f(r); return r.Validate() }
	for name, f := range map[string]func(r *RuleSet){
		"bad name":                        func(r *RuleSet) { r.Name = "a b" },
		"bad event type":                  func(r *RuleSet) { r.EventType = "invoice" },
		"bad book type":                   func(r *RuleSet) { r.BookType = "SECRET" },
		"bad version":                     func(r *RuleSet) { r.Version = "v1" },
		"no test pack":                    func(r *RuleSet) { r.TestPack = nil },
		"end before from":                 func(r *RuleSet) { x := d0.Add(-time.Hour); r.EffectiveTo = &x },
		"unknown rounding":                func(r *RuleSet) { r.Content.RoundingPolicyID = "NOPE" },
		"residual policy without account": func(r *RuleSet) { r.Content.RoundingPolicyID = "HALF_EVEN_RESIDUAL" },
		"only debits": func(r *RuleSet) {
			r.Content.LineTemplates = []LineTemplate{{LineRef: "A", Side: Debit, Account: AccountRef{AccountCode: "1"}, Amount: "gross"}, {LineRef: "B", Side: Debit, Account: AccountRef{AccountCode: "2"}, Amount: "gross"}}
		},
		"both account forms": func(r *RuleSet) { r.Content.LineTemplates[0].Account = AccountRef{AccountCode: "1", MappingKey: "AR"} },
		"bad expression":     func(r *RuleSet) { r.Content.LineTemplates[0].Amount = "gross +" },
		"duplicate line ref": func(r *RuleSet) { r.Content.LineTemplates[1].LineRef = "L1" },
		"bad condition op":   func(r *RuleSet) { r.Content.Conditions[0].Op = "like" },
		"gt needs decimal":   func(r *RuleSet) { r.Content.LineTemplates[2].When[0].Value = "lots" },
		"in needs list":      func(r *RuleSet) { r.Content.Conditions = []Condition{{Fact: "x", Op: "in", Value: "a"}} },
		"derived needs from": func(r *RuleSet) { r.Content.DimensionRules = []DimensionRule{{Dimension: "p", Mode: DimDerived}} },
		"unknown dim mode":   func(r *RuleSet) { r.Content.DimensionRules = []DimensionRule{{Dimension: "p", Mode: "MAYBE"}} },
		"empty tax facts":    func(r *RuleSet) { r.Content.TaxDependency = &TaxDependency{} },
	} {
		if err := mut(f); err == nil {
			t.Errorf("%s must be refused", name)
		} else if e, ok := AsException(err); !ok || e.Family != FamRule {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEventAndBookValidation(t *testing.T) {
	if err := event(okFacts()).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(e *AccountingEvent){
		"type":      func(e *AccountingEvent) { e.EventType = "invoice" },
		"version":   func(e *AccountingEvent) { e.EventVersion = 0 },
		"entity":    func(e *AccountingEvent) { e.LegalEntityID = "x" },
		"source":    func(e *AccountingEvent) { e.SourceEventID = "" },
		"idem":      func(e *AccountingEvent) { e.IdempotencyKey = "" },
		"causation": func(e *AccountingEvent) { e.CausationID = " " },
		"future":    func(e *AccountingEvent) { e.OccurredAt = time.Now().Add(72 * time.Hour) },
		"currency":  func(e *AccountingEvent) { e.TransactionCurrency = "gbp" },
		"no ccy":    func(e *AccountingEvent) { e.TransactionCurrency = "" },
		"fact name": func(e *AccountingEvent) { e.Facts["Bad Name"] = "1" },
		"long fact": func(e *AccountingEvent) { e.Facts["x"] = strings.Repeat("a", 600) },
	} {
		e := event(okFacts())
		f(e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	e := event(okFacts())
	h := e.ComputeHash()
	e.Facts["gross"] = "121.00"
	if e.ComputeHash() == h {
		t.Fatal("the hash must cover the facts")
	}
	e = event(okFacts())
	e.IdempotencyKey, e.CorrelationID = "other", "other"
	if e.ComputeHash() != h {
		t.Fatal("delivery metadata is not part of what the event is")
	}
	if len(NewEventID()) != 36 || NewEventID() == NewEventID() {
		t.Fatal("event ids are unique UUIDs")
	}
	b := book()
	if err := b.ValidateNew(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(b *Book){
		"code": func(b *Book) { b.BookCode = "" }, "type": func(b *Book) { b.BookType = "X" }, "ccy": func(b *Book) { b.FunctionalCurrency = "gb" },
		"framework": func(b *Book) { b.AccountingFrameworkID = "free text with spaces" }, "entity": func(b *Book) { b.LegalEntityID = "" },
		"from": func(b *Book) { b.EffectiveFrom = time.Time{} }, "to": func(b *Book) { x := d0.Add(-time.Hour); b.EffectiveTo = &x },
	} {
		bb := book()
		f(bb)
		if err := bb.ValidateNew(); err == nil {
			t.Errorf("book %s must be refused", name)
		}
	}
	end := d0.AddDate(1, 0, 0)
	b.EffectiveTo = &end
	if !b.InForce(d0.AddDate(0, 6, 0)) || b.InForce(end) || b.InForce(d0.Add(-time.Hour)) {
		t.Fatal("a book carries postings only inside its effective interval")
	}
}

func TestTestPackAndLifecycle(t *testing.T) {
	rs := rule(invoiceContent())
	if failed := RunTestPack(context.Background(), rs, *book()); len(failed) != 0 {
		t.Fatalf("%v", failed)
	}
	rs.TestPack[0].Expect.Lines[0].Amount = "121.00"
	if failed := RunTestPack(context.Background(), rs, *book()); len(failed) != 1 || !strings.Contains(failed[0], "basic") {
		t.Fatalf("%v", failed)
	}
	rs = rule(invoiceContent())
	rs.TestPack = append(rs.TestPack, TestCase{Name: "missing tax fact", Event: TestEvent{EventType: "AR.INVOICE_ISSUED", TransactionCurrency: "GBP", Facts: map[string]string{"document_type": "INVOICE", "gross": "1.00", "net": "1.00"}},
		Accounts: map[string]string{"AR": "1100", "REV": "4000", "VAT": "2200"}, Expect: TestExpectation{Lines: []ExpectedLine{{Side: Debit, AccountCode: "1100", Amount: "1.00"}, {Side: Credit, AccountCode: "4000", Amount: "1.00"}}}})
	rs.TestPack = append(rs.TestPack, TestCase{Name: "no branch", Event: TestEvent{EventType: "AR.INVOICE_ISSUED", TransactionCurrency: "GBP", Facts: map[string]string{"document_type": "CREDIT_NOTE", "gross": "1.00", "net": "1.00", "tax": "0"}},
		Accounts: map[string]string{"AR": "1100", "REV": "4000", "VAT": "2200"}, Expect: TestExpectation{Exception: FamRule}})
	if failed := RunTestPack(context.Background(), rs, *book()); len(failed) != 0 {
		t.Fatalf("%v", failed)
	}
	if HashContent(rs.Content, rs.TestPack) == HashContent(rs.Content, rs.TestPack[:1]) {
		t.Fatal("the hash binds the test pack")
	}
	for from, to := range map[string]string{RuleDraft: RuleTest, RuleTest: RuleApproved, RuleApproved: RuleActive, RuleActive: RuleSuperseded, RuleSuperseded: RuleRetired} {
		if !CanTransition(from, to) {
			t.Errorf("%s -> %s", from, to)
		}
	}
	for _, p := range [][2]string{{RuleDraft, RuleActive}, {RuleDraft, RuleApproved}, {RuleRetired, RuleActive}, {RuleActive, RuleDraft}, {RuleSuperseded, RuleActive}} {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s must not be allowed", p[0], p[1])
		}
	}
	a, b := rule(invoiceContent()), rule(invoiceContent())
	b.RuleSetID, b.Version = "r2", "1.1.0"
	if !a.Overlaps(b) {
		t.Fatal("two open-ended rule sets for one scope overlap")
	}
	end := d0.AddDate(0, 6, 0)
	a.EffectiveTo = &end
	b.EffectiveFrom = end
	if a.Overlaps(b) {
		t.Fatal("back-to-back rule sets do not overlap")
	}
	b.EventType = "AP.INVOICE_APPROVED"
	b.EffectiveFrom = d0
	if a.Overlaps(b) {
		t.Fatal("different scopes never overlap")
	}
}
