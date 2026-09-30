package domain

import (
	"encoding/json"
	"math/big"
	"testing"
)

func slip(id, gross, tax, ben, net string) PopulationRecord {
	r := rec(id, "RUN-1", net, "USD", "2026-09-25")
	r.Attributes = map[string]string{"gross_pay": gross, "tax_withheld": tax, "benefits_deductions": ben, "net_pay": net}
	return r
}

func g2n(t *testing.T) RuleLogic {
	t.Helper()
	l, err := ParseRuleLogic(json.RawMessage(`{"kind":"ARITHMETIC","equations":[{"name":"gross_to_net",
		"plus":["attr:gross_pay"],"minus":["attr:tax_withheld","attr:benefits_deductions"],"equals":"attr:net_pay"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestArithmetic_FootingSlipsPass(t *testing.T) {
	o, err := CheckArithmetic([]PopulationRecord{
		slip("s1", "5000.00", "1200.00", "300.50", "3499.50"),
		slip("s2", "4000", "900", "0", "3100.00"),
	}, g2n(t), new(big.Rat))
	if err != nil || o.Result() != ResultPass {
		t.Fatalf("got %+v %v", o.Exceptions, err)
	}
}

// Scenario 17 flavour: one slip does not foot even though the run could still tie in total.
func TestArithmetic_OneBadSlipFailsAtRecordLevel(t *testing.T) {
	o, _ := CheckArithmetic([]PopulationRecord{
		slip("s1", "5000", "1200", "300", "3500"),
		slip("s2", "4000", "900", "0", "3200"), // 4000-900 = 3100, not 3200
	}, g2n(t), new(big.Rat))
	if o.Result() != ResultFail || len(o.Exceptions) != 1 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	e := o.Exceptions[0]
	if e.Reason != ReasonEquationViolation || e.RecordIDs[0] != "s2" || e.Exposure != "100" || e.Category != CatMismatch || e.Assertion != "ACCURACY" {
		t.Errorf("unexpected exception %+v", e)
	}
}

func TestArithmetic_OffsettingErrorsAcrossSlipsStillFail(t *testing.T) {
	// +50 on one slip, −50 on another: run totals tie, individual slips do not.
	o, _ := CheckArithmetic([]PopulationRecord{
		slip("s1", "5000", "1000", "0", "4050"),
		slip("s2", "5000", "1000", "0", "3950"),
	}, g2n(t), new(big.Rat))
	if len(o.Exceptions) != 2 {
		t.Fatalf("both slips must be flagged, got %+v", o.Exceptions)
	}
}

func TestArithmetic_UsesThePinnedTolerance_ExactDecimals(t *testing.T) {
	recs := []PopulationRecord{slip("s1", "100.10", "0", "0", "100.09")}
	tol, _ := new(big.Rat).SetString("0.01")
	if o, _ := CheckArithmetic(recs, g2n(t), tol); o.Result() != ResultPass {
		t.Fatalf("0.01 within 0.01 (boundary): %+v", o.Exceptions)
	}
	tight, _ := new(big.Rat).SetString("0.009")
	if o, _ := CheckArithmetic(recs, g2n(t), tight); o.Result() != ResultFail {
		t.Fatal("0.01 exceeds 0.009")
	}
	// float64 would call 0.1+0.2-0.3 nonzero.
	f := []PopulationRecord{slip("s1", "0.3", "0.1", "0.2", "0")}
	if o, _ := CheckArithmetic(f, g2n(t), new(big.Rat)); o.Result() != ResultPass {
		t.Fatalf("exact decimals: %+v", o.Exceptions)
	}
}

func TestArithmetic_MissingOrInvalidOperandIsAnExceptionNotASkip(t *testing.T) {
	missing := slip("s1", "5000", "1000", "0", "4000")
	delete(missing.Attributes, "tax_withheld")
	bad := slip("s2", "5000", "1000", "0", "4000")
	bad.Attributes["net_pay"] = "4,000"
	o, _ := CheckArithmetic([]PopulationRecord{missing, bad}, g2n(t), new(big.Rat))
	if o.Result() != ResultFail || len(o.Exceptions) != 2 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	for _, e := range o.Exceptions {
		if e.Reason != ReasonOperandInvalid || e.Category != CatDataQuality {
			t.Errorf("unexpected %+v", e)
		}
	}
}

func TestArithmetic_AmountOperandAndMultipleEquations(t *testing.T) {
	l, err := ParseRuleLogic(json.RawMessage(`{"kind":"ARITHMETIC","equations":[
		{"name":"a","plus":["amount"],"equals":"attr:net_pay"},
		{"name":"b","plus":["attr:gross_pay"],"minus":["attr:tax_withheld"],"equals":"amount"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ok := slip("s1", "100", "20", "0", "80") // amount = net = 80; 100-20 = 80
	if o, _ := CheckArithmetic([]PopulationRecord{ok}, l, new(big.Rat)); o.Result() != ResultPass {
		t.Fatalf("got %+v", o.Exceptions)
	}
	bad := slip("s2", "100", "10", "0", "80") // second equation: 100-10 = 90 != 80
	if o, _ := CheckArithmetic([]PopulationRecord{bad}, l, new(big.Rat)); len(o.Exceptions) != 1 {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestArithmetic_FindingLimitDisclosed(t *testing.T) {
	l, _ := ParseRuleLogic(json.RawMessage(`{"kind":"ARITHMETIC","max_findings":2,"equations":[{"name":"n","plus":["attr:gross_pay"],"equals":"attr:net_pay"}]}`))
	var recs []PopulationRecord
	for i := 0; i < 5; i++ {
		recs = append(recs, slip("s"+string(rune('a'+i)), "10", "0", "0", "9"))
	}
	o, _ := CheckArithmetic(recs, l, new(big.Rat))
	limit := 0
	for _, e := range o.Exceptions {
		if e.Reason == ReasonFindingLimit {
			limit++
		}
	}
	if len(o.Exceptions) != 3 || limit != 1 {
		t.Fatalf("2 findings + 1 disclosure expected, got %+v", o.Exceptions)
	}
}

func TestArithmetic_Deterministic(t *testing.T) {
	a := []PopulationRecord{slip("s2", "10", "0", "0", "9"), slip("s1", "10", "0", "0", "8")}
	b := []PopulationRecord{a[1], a[0]}
	x, _ := CheckArithmetic(a, g2n(t), new(big.Rat))
	y, _ := CheckArithmetic(b, g2n(t), new(big.Rat))
	j1, _ := json.Marshal(x)
	j2, _ := json.Marshal(y)
	if string(j1) != string(j2) {
		t.Fatal("output must not depend on input order")
	}
}

func TestArithmetic_LogicValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"no equations":      `{"kind":"ARITHMETIC"}`,
		"no name":           `{"kind":"ARITHMETIC","equations":[{"plus":["amount"],"equals":"amount"}]}`,
		"no plus":           `{"kind":"ARITHMETIC","equations":[{"name":"x","equals":"amount"}]}`,
		"bad operand":       `{"kind":"ARITHMETIC","equations":[{"name":"x","plus":["gross_pay"],"equals":"amount"}]}`,
		"injection operand": `{"kind":"ARITHMETIC","equations":[{"name":"x","plus":["attr:a;drop"],"equals":"amount"}]}`,
	} {
		if _, err := ParseRuleLogic(json.RawMessage(raw)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

// ── exclusions ───────────────────────────────────────────────────────────────

func TestExclusions_ImpactPerRuleAndCurrency(t *testing.T) {
	recs := []PopulationRecord{
		withAttr(rec("a", "R", "10", "USD", "2026-09-01"), "run_status", "BLOCKED"),
		withAttr(rec("b", "R", "5", "USD", "2026-09-01"), "run_status", "BLOCKED"),
		withAttr(rec("c", "R", "7", "EUR", "2026-09-01"), "run_status", "BLOCKED"),
		withAttr(rec("d", "R", "100", "USD", "2026-09-01"), "run_status", "COMPLETED"),
	}
	rules := []ExclusionRule{{Attr: "run_status", Values: []string{"BLOCKED", "INITIATED"}, Reason: "not final", Authority: "CONTROLLER"}}
	kept, ex, err := ApplyExclusions(recs, rules, SideA)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].RecordID != "d" {
		t.Fatalf("only the COMPLETED record survives, got %+v", kept)
	}
	if len(ex) != 2 || ex[0].Currency != "EUR" || ex[0].Count != 1 || ex[0].Value != "7" || ex[1].Currency != "USD" || ex[1].Count != 2 || ex[1].Value != "15" {
		t.Fatalf("impact must be reported per currency, got %+v", ex)
	}
	for _, e := range ex {
		if e.Authority != "CONTROLLER" || !ValidDigest(e.Digest) {
			t.Errorf("exclusion must carry its authority and an ids digest: %+v", e)
		}
	}
}

func TestExclusions_SideScopingAndNoMatchAndNoRules(t *testing.T) {
	recs := []PopulationRecord{withAttr(rec("a", "R", "1", "USD", "2026-09-01"), "s", "X")}
	rule := ExclusionRule{Side: "B", Attr: "s", Values: []string{"X"}, Reason: "r", Authority: "a"}
	if kept, ex, _ := ApplyExclusions(recs, []ExclusionRule{rule}, SideA); len(kept) != 1 || len(ex) != 0 {
		t.Fatal("a side-B rule must not touch side A")
	}
	if kept, ex, _ := ApplyExclusions(recs, []ExclusionRule{rule}, SideB); len(kept) != 0 || len(ex) != 1 {
		t.Fatal("a side-B rule excludes on side B")
	}
	if kept, ex, _ := ApplyExclusions(recs, nil, SideA); len(kept) != 1 || ex == nil || len(ex) != 0 {
		t.Fatal("no rules: unchanged, and an empty (non-nil) exclusion list")
	}
	noAttr := []PopulationRecord{rec("z", "R", "1", "USD", "2026-09-01")}
	if kept, _, _ := ApplyExclusions(noAttr, []ExclusionRule{{Attr: "s", Values: []string{"X"}, Reason: "r", Authority: "a"}}, SideA); len(kept) != 1 {
		t.Fatal("a record without the attribute is never excluded")
	}
}

func TestExclusions_FirstMatchingRuleOwnsARecord(t *testing.T) {
	r := withAttr(rec("a", "R", "10", "USD", "2026-09-01"), "s", "X", "t", "Y")
	rules := []ExclusionRule{
		{Attr: "s", Values: []string{"X"}, Reason: "first", Authority: "A1"},
		{Attr: "t", Values: []string{"Y"}, Reason: "second", Authority: "A2"},
	}
	_, ex, _ := ApplyExclusions([]PopulationRecord{r}, rules, SideA)
	if len(ex) != 1 || ex[0].Authority != "A1" {
		t.Fatalf("a record is counted once, under the first rule: %+v", ex)
	}
}
