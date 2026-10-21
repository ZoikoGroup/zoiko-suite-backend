package kit

import (
	"encoding/json"
	"testing"
)

func TestParseAmount(t *testing.T) {
	cases := map[string]string{
		"0": "0.0000", "12": "12.0000", "12.5": "12.5000", "-0.0001": "-0.0001",
		"+7.25": "7.2500", ".5": "0.5000", "1234567.8901": "1234567.8901",
	}
	for in, want := range cases {
		got, err := ParseAmount(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseAmount(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.23456", "1e3", "1,000", "--1", "1.2.3", "."} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
}

// 0.1 + 0.2 is the canonical float trap; the exact type must give 0.3.
func TestAmountIsExact(t *testing.T) {
	if got := MustAmount("0.1").Add(MustAmount("0.2")); got != MustAmount("0.3") {
		t.Fatalf("0.1+0.2 = %s", got)
	}
	var sum Amount
	for i := 0; i < 1000; i++ {
		sum += MustAmount("0.01")
	}
	if sum != MustAmount("10") {
		t.Fatalf("1000 x 0.01 = %s", sum)
	}
}

func TestAmountJSON(t *testing.T) {
	var v struct {
		A Amount `json:"a"`
		B Amount `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"10.50","b":0.1}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != MustAmount("10.5") || v.B != MustAmount("0.1") {
		t.Fatalf("got %v %v", v.A, v.B)
	}
	out, _ := json.Marshal(v)
	if string(out) != `{"a":"10.5000","b":"0.1000"}` {
		t.Fatalf("marshal = %s", out)
	}
	if err := json.Unmarshal([]byte(`{"a":"1.23456"}`), &v); err == nil {
		t.Fatal("5 fractional digits must be refused, not rounded")
	}
}

func TestCurrencyPrecision(t *testing.T) {
	if !MustAmount("12.34").FitsCurrency("USD") || MustAmount("12.345").FitsCurrency("USD") {
		t.Error("USD has 2 minor digits")
	}
	if !MustAmount("1200").FitsCurrency("JPY") || MustAmount("1200.5").FitsCurrency("JPY") {
		t.Error("JPY has 0 minor digits")
	}
	if !MustAmount("1.234").FitsCurrency("KWD") || MustAmount("1.2345").FitsCurrency("KWD") {
		t.Error("KWD has 3 minor digits")
	}
	if got := MustAmount("2.345").RoundHalfUp("USD"); got != MustAmount("2.35") {
		t.Errorf("round half up: %s", got)
	}
	if got := MustAmount("-2.345").RoundHalfUp("USD"); got != MustAmount("-2.35") {
		t.Errorf("round half away from zero: %s", got)
	}
}

func TestPostingEventValidate(t *testing.T) {
	ok := PostingEvent{SourceEventID: "x", Lines: []PostingLine{
		{MappingKey: "AR_RECEIVABLE_STANDARD", Debit: MustAmount("120")},
		{MappingKey: "AR_REVENUE_STANDARD", Credit: MustAmount("100")},
		{MappingKey: "AR_TAX_OUTPUT", Credit: MustAmount("20")},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("balanced event refused: %v", err)
	}
	if ok.Lines[0].DebitAmount != 120 || ok.Lines[2].CreditAmount != 20 {
		t.Fatal("wire floats not populated")
	}
	unbalanced := PostingEvent{Lines: []PostingLine{
		{MappingKey: "A", Debit: MustAmount("100")}, {MappingKey: "B", Credit: MustAmount("99.9999")}}}
	if err := unbalanced.Validate(); err == nil {
		t.Fatal("unbalanced event accepted")
	}
	both := PostingEvent{Lines: []PostingLine{
		{MappingKey: "A", Debit: MustAmount("1"), Credit: MustAmount("1")}, {MappingKey: "B", Credit: MustAmount("1")}}}
	if err := both.Validate(); err == nil {
		t.Fatal("a line with both sides accepted")
	}
}
