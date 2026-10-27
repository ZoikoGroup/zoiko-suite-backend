package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }
func iptr(v int) *int    { return &v }

var day0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func mi(id, side, ref string, amt int64, ccy string) MatchItem {
	return MatchItem{ItemID: "i-" + id, Side: side, SourceRecordID: id, RefID: ref, Amount: amt, Currency: ccy, OccurredAt: day0}
}

func abs(ccy string, v int64) TolerancePolicy {
	return TolerancePolicy{Currencies: map[string]CurrencyTolerance{ccy: {AbsoluteMinorUnits: i64(v)}}}
}

func TestValidateMatchKeyFields(t *testing.T) {
	for _, ok := range [][]string{{"ref_id"}, {"ref_id", "dimensions.account_code"}, {"currency", "occurred_at", "amount_minor_units"}} {
		if err := ValidateMatchKeyFields(ok); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{nil, {"reference"}, {"dimensions."}, {"dimensions.Bad-Name"}, {"ref_id", "ref_id"}} {
		if err := ValidateMatchKeyFields(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
}

func TestTolerancePolicy_Validate(t *testing.T) {
	sides := []string{"Bank", "Ledger"}
	good := []TolerancePolicy{
		{},
		abs("USD", 5),
		{Currencies: map[string]CurrencyTolerance{"EUR": {PercentageBps: i64(10)}}, PercentageBaseSide: "Ledger", DateDays: iptr(3)},
	}
	for _, p := range good {
		if err := p.Validate(sides); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	bad := map[string]TolerancePolicy{
		"both kinds":       {Currencies: map[string]CurrencyTolerance{"USD": {AbsoluteMinorUnits: i64(1), PercentageBps: i64(1)}}},
		"neither kind":     {Currencies: map[string]CurrencyTolerance{"USD": {}}},
		"not a currency":   abs("usd", 1),
		"negative":         abs("USD", -1),
		"pct without base": {Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: i64(10)}}},
		"pct unknown base": {Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: i64(10)}}, PercentageBaseSide: "GL"},
		"pct over 100%":    {Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: i64(10001)}}, PercentageBaseSide: "Bank"},
		"base without pct": {PercentageBaseSide: "Bank"},
		"negative day":     {DateDays: iptr(-1)},
	}
	for name, p := range bad {
		if err := p.Validate(sides); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

// A5: absolute, percentage (of an explicit base, floored) and date tolerance.
func TestCompare_Tolerances(t *testing.T) {
	a, b := mi("a", "Bank", "R", 10000, "USD"), mi("b", "Ledger", "R", 10050, "USD")
	if r, _ := Compare(a, b, abs("USD", 50)); r != "" {
		t.Fatalf("within absolute: %s", r)
	}
	if r, d := Compare(a, b, abs("USD", 49)); r == "" || d.Rule != RuleAmountTolerance || *d.DifferenceMinor != 50 || *d.AllowedMinor != 49 {
		t.Fatalf("outside absolute: %q %+v", r, d)
	}
	if r, _ := Compare(a, b, TolerancePolicy{}); r == "" {
		t.Fatal("a currency without a declared tolerance must match exactly")
	}

	// 50 bps of the Ledger amount 10050 = 50.25 -> floored to 50: passes; of the
	// Bank amount 10000 = 50: passes; 49 bps of 10050 = 49.24 -> 49: fails.
	pct := func(bps int64, base string) TolerancePolicy {
		return TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: i64(bps)}}, PercentageBaseSide: base}
	}
	if r, _ := Compare(a, b, pct(50, "Ledger")); r != "" {
		t.Fatalf("within 50bps of Ledger: %s", r)
	}
	if r, _ := Compare(a, b, pct(50, "Bank")); r != "" {
		t.Fatalf("within 50bps of Bank: %s", r)
	}
	if r, d := Compare(a, b, pct(49, "Ledger")); r == "" || *d.AllowedMinor != 49 {
		t.Fatalf("outside 49bps: %q %+v", r, d)
	}

	late := b
	late.Amount = 10000
	late.OccurredAt = day0.AddDate(0, 0, 4)
	if r, _ := Compare(a, late, TolerancePolicy{}); r != "" {
		t.Fatalf("dates are not compared without a date tolerance: %s", r)
	}
	if r, _ := Compare(a, late, TolerancePolicy{DateDays: iptr(4)}); r != "" {
		t.Fatalf("4 days within 4: %s", r)
	}
	if r, d := Compare(a, late, TolerancePolicy{DateDays: iptr(3)}); r == "" || d.Rule != RuleDateTolerance || *d.DateDifferenceDays != 4 {
		t.Fatalf("4 days outside 3: %q %+v", r, d)
	}
}

// A2: different currencies are a mismatch, never an amount comparison — even
// when the amounts are identical and the tolerance is generous.
func TestCompare_CurrencyMismatch(t *testing.T) {
	p := TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {AbsoluteMinorUnits: i64(1 << 40)}, "EUR": {AbsoluteMinorUnits: i64(1 << 40)}}}
	r, d := Compare(mi("a", "Bank", "R", 100, "USD"), mi("b", "Ledger", "R", 100, "EUR"), p)
	if r == "" || d.Rule != RuleCurrencyMismatch {
		t.Fatalf("currency mismatch: %q %+v", r, d)
	}
}

func TestCompare_ExtremeAmountsDoNotWrap(t *testing.T) {
	r, _ := Compare(mi("a", "Bank", "R", math.MaxInt64, "USD"), mi("b", "Ledger", "R", math.MinInt64, "USD"), abs("USD", math.MaxInt64))
	if r == "" {
		t.Fatal("a difference beyond int64 was treated as within tolerance")
	}
}

// A3: the declared composite key decides pairing; ref_id alone does not.
func TestAutoMatch_CompositeKey(t *testing.T) {
	withAcct := func(it MatchItem, acct string) MatchItem {
		it.Dimensions = map[string]string{"account_code": acct}
		return it
	}
	a := []MatchItem{withAcct(mi("a1", "Bank", "R1", 100, "USD"), "1100"), withAcct(mi("a2", "Bank", "R1", 200, "USD"), "1200")}
	b := []MatchItem{withAcct(mi("b1", "Ledger", "R1", 200, "USD"), "1200"), withAcct(mi("b2", "Ledger", "R1", 100, "USD"), "1100")}

	pairs, exc := AutoMatch("Bank", "Ledger", a, b, []string{"ref_id", "dimensions.account_code"}, TolerancePolicy{})
	if len(exc) != 0 || len(pairs) != 2 {
		t.Fatalf("composite key: %d pairs, exceptions %+v", len(pairs), exc)
	}
	for _, p := range pairs {
		if p.A.Dimensions["account_code"] != p.B.Dimensions["account_code"] {
			t.Fatalf("paired across account codes: %+v", p)
		}
	}

	// On ref_id alone the first Ledger candidate that reconciles is chosen, so
	// the result is still exact here — but only because amounts differ.
	pairs, exc = AutoMatch("Bank", "Ledger", a, b, []string{"ref_id"}, TolerancePolicy{})
	if len(exc) != 0 || len(pairs) != 2 {
		t.Fatalf("ref_id only: %d pairs, %+v", len(pairs), exc)
	}
}

func TestAutoMatch_MissingKeyFieldIsAnException(t *testing.T) {
	a := []MatchItem{mi("a1", "Bank", "R1", 100, "USD")}
	b := []MatchItem{mi("b1", "Ledger", "R1", 100, "USD")}
	pairs, exc := AutoMatch("Bank", "Ledger", a, b, []string{"ref_id", "dimensions.account_code"}, TolerancePolicy{})
	if len(pairs) != 0 || len(exc) != 2 || exc[0].Detail.Rule != RuleMissingKey {
		t.Fatalf("items missing a key field paired on the empty value: %d pairs, %+v", len(pairs), exc)
	}
}

func TestAutoMatch_MismatchesAndLeftovers(t *testing.T) {
	a := []MatchItem{mi("a1", "Bank", "R1", 100, "USD"), mi("a2", "Bank", "R2", 100, "USD"), mi("a3", "Bank", "R3", 100, "USD")}
	b := []MatchItem{mi("b1", "Ledger", "R1", 100, "EUR"), mi("b2", "Ledger", "R2", 150, "USD"), mi("b4", "Ledger", "R4", 1, "USD")}
	pairs, exc := AutoMatch("Bank", "Ledger", a, b, []string{"ref_id"}, abs("USD", 10))
	if len(pairs) != 0 {
		t.Fatalf("pairs: %+v", pairs)
	}
	rules := map[string]string{}
	for _, e := range exc {
		rules[e.Item.SourceRecordID] = e.Detail.Rule
	}
	want := map[string]string{"a1": RuleCurrencyMismatch, "b1": RuleCurrencyMismatch, "a2": RuleAmountTolerance, "b2": RuleAmountTolerance,
		"a3": RuleUnmatched, "b4": RuleUnmatched}
	if len(rules) != len(want) {
		t.Fatalf("exceptions: %+v", rules)
	}
	for k, v := range want {
		if rules[k] != v {
			t.Fatalf("%s: rule %s, want %s (all %+v)", k, rules[k], v, rules)
		}
	}
}

// DQC-03 / NP-21: the aggregate balances, a sub-dimension does not — the
// variance is reported even though every item pair matched.
func TestCheckBalances_SubDimensionCannotHideInAggregate(t *testing.T) {
	dim := func(it MatchItem, v string) MatchItem { it.Dimensions = map[string]string{"entity": v}; return it }
	a := []MatchItem{dim(mi("a1", "Bank", "R1", 100, "USD"), "E1"), dim(mi("a2", "Bank", "R2", 100, "USD"), "E2")}
	b := []MatchItem{dim(mi("b1", "Ledger", "R1", 100, "USD"), "E2"), dim(mi("b2", "Ledger", "R2", 100, "USD"), "E1")}
	b[0].Dimensions["entity"], b[1].Dimensions["entity"] = "E1", "E1" // Ledger books both to E1

	pairs, exc := AutoMatch("Bank", "Ledger", a, b, []string{"ref_id"}, TolerancePolicy{})
	if len(pairs) != 2 || len(exc) != 0 {
		t.Fatalf("item matching should pass on ref_id: %d pairs %+v", len(pairs), exc)
	}
	v, err := CheckBalances("Bank", "Ledger", a, b, []string{"entity"}, TolerancePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 2 {
		t.Fatalf("variances: %+v", v)
	}
	for _, x := range v {
		if x.Dimension != "entity" || x.Currency != "USD" {
			t.Fatalf("aggregate reported or wrong bucket: %+v", x)
		}
	}
	if v[0].DimensionValue != "E1" || v[0].Difference != -100 || v[1].DimensionValue != "E2" || v[1].Difference != 100 {
		t.Fatalf("variances: %+v", v)
	}
}

// A2: totals are per currency — a USD shortfall cannot be offset by EUR.
func TestCheckBalances_PerCurrency(t *testing.T) {
	a := []MatchItem{mi("a1", "Bank", "R1", 100, "USD"), mi("a2", "Bank", "R2", 50, "EUR")}
	b := []MatchItem{mi("b1", "Ledger", "R1", 150, "USD")}
	v, err := CheckBalances("Bank", "Ledger", a, b, nil, abs("USD", 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 2 || v[0].Currency != "EUR" || v[0].Difference != 50 || v[1].Currency != "USD" || v[1].Difference != -50 {
		t.Fatalf("per-currency variances: %+v", v)
	}
}

func TestCheckBalances_ToleranceAndOverflow(t *testing.T) {
	a := []MatchItem{mi("a1", "Bank", "R1", 1000, "USD")}
	b := []MatchItem{mi("b1", "Ledger", "R1", 990, "USD")}
	if v, _ := CheckBalances("Bank", "Ledger", a, b, nil, abs("USD", 10)); len(v) != 0 {
		t.Fatalf("within tolerance reported: %+v", v)
	}
	pct := TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: i64(100)}}, PercentageBaseSide: "Ledger"}
	if v, _ := CheckBalances("Bank", "Ledger", a, b, nil, pct); len(v) != 1 || v[0].Allowed != 9 {
		t.Fatalf("1%% of the Ledger total 990 floors to 9: %+v", v)
	}
	big := []MatchItem{mi("x", "Bank", "R", math.MaxInt64, "USD"), mi("y", "Bank", "S", 1, "USD")}
	if _, err := CheckBalances("Bank", "Ledger", big, nil, nil, TolerancePolicy{}); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflowing total: %v", err)
	}
}
