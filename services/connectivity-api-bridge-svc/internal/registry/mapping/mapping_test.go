package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func ip(n int) *int       { return &n }
func sp(s string) *string { return &s }

func invoiceSpec() Spec {
	return Spec{
		CanonicalContract: "tax.invoice.v1",
		Fields: []Field{
			{To: "/amount", From: "/total/value", Type: TypeDecimal, Scale: ip(2), Required: true},
			{To: "/currency", From: "/total/ccy", Type: TypeEnum, Required: true, Enum: map[string]string{"GBP": "GBP", "USD": "USD", "eur": "EUR"}},
			{To: "/issued_at", From: "/issued", Type: TypeDatetime, Required: true, KeepSourceOffset: true},
			{To: "/due_date", From: "/due", Type: TypeDate},
			{To: "/supplier/ref", From: "/supplier/id", Type: TypeString, Required: true, Trim: true, MaxLength: ip(10)},
			{To: "/lines_count", From: "/lines", Type: TypeInteger},
			{To: "/paid", From: "/paid", Type: TypeBoolean},
			{To: "/source", Type: TypeString, Const: sp("xic"), NonBusinessDefault: true},
		},
		ProhibitedSourcePaths: []string{"/payer/national_id"},
	}
}

func mustCompile(t *testing.T, s Spec) *Compiled {
	t.Helper()
	c, err := Compile(s)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

const goodDoc = `{"total":{"value":"1200.50","ccy":"GBP"},"issued":"2026-03-01T10:00:00+01:00","due":"2026-04-01",
 "supplier":{"id":"  S-1 "},"lines":3,"paid":false,"internal_note":"drop me","audit":{"x":1}}`

func eval(t *testing.T, c *Compiled, doc string) *Result {
	t.Helper()
	return c.Evaluate([]byte(doc), "mv-1", 0)
}

func codes(r *Result) []string {
	var out []string
	for _, i := range r.Issues {
		out = append(out, string(i.Class)+":"+i.Code+":"+i.Field)
	}
	return out
}

func TestMapsAGoodDocument(t *testing.T) {
	r := eval(t, mustCompile(t, invoiceSpec()), goodDoc)
	if !r.OK() {
		t.Fatalf("issues: %v", codes(r))
	}
	want := `{"amount":"1200.50","currency":"GBP","due_date":"2026-04-01","issued_at":"2026-03-01T09:00:00Z","issued_at_source_offset":"+01:00","lines_count":3,"paid":false,"source":"xic","supplier":{"ref":"S-1"}}`
	if string(r.Canonical) != want {
		t.Fatalf("canonical\n got %s\nwant %s", r.Canonical, want)
	}
	if r.CanonicalHash == "" || r.DroppedTopLevel != 2 {
		t.Fatalf("hash=%q dropped=%d", r.CanonicalHash, r.DroppedTopLevel)
	}
}

func TestDeterministic(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	first := eval(t, c, goodDoc)
	for i := 0; i < 200; i++ {
		r := eval(t, c, goodDoc)
		if r.CanonicalHash != first.CanonicalHash || string(r.Canonical) != string(first.Canonical) {
			t.Fatalf("run %d differs", i)
		}
	}
	// a different source key order is the same source
	reordered := `{"paid":false,"lines":3,"supplier":{"id":"  S-1 "},"due":"2026-04-01","issued":"2026-03-01T10:00:00+01:00","total":{"ccy":"GBP","value":"1200.50"}}`
	if r := eval(t, c, reordered); r.CanonicalHash != first.CanonicalHash {
		t.Fatal("key order changed the canonical payload")
	}
}

func TestMinimization_UnnamedSourceFieldsNeverArrive(t *testing.T) {
	r := eval(t, mustCompile(t, invoiceSpec()), goodDoc)
	if strings.Contains(string(r.Canonical), "drop me") || strings.Contains(string(r.Canonical), "audit") {
		t.Fatalf("unnamed field leaked: %s", r.Canonical)
	}
}

func TestDecimal_NoSilentRounding_NP10(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	for _, v := range []string{`"10.001"`, `10.005`, `"0.1234"`} {
		r := eval(t, c, fmt.Sprintf(`{"total":{"value":%s,"ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`, v))
		if r.OK() || r.Issues[0].Code != "precision_loss" {
			t.Fatalf("%s: %v", v, codes(r))
		}
	}
	// trailing zeros are not loss; the output is normalized to the scale
	r := eval(t, c, `{"total":{"value":12.500,"ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if !r.OK() || !strings.Contains(string(r.Canonical), `"amount":"12.50"`) {
		t.Fatalf("normalization: %v %s", codes(r), r.Canonical)
	}
	// a float-looking JSON number keeps its exact text: 0.1 stays 0.10, never 0.1000000000000000055
	r = eval(t, c, `{"total":{"value":0.1,"ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if !strings.Contains(string(r.Canonical), `"amount":"0.10"`) {
		t.Fatalf("float handling: %s", r.Canonical)
	}
}

func TestDecimal_RefusesExponentsAndGarbage(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	for _, v := range []string{`1e3`, `"1e3"`, `"abc"`, `"1,000.00"`, `"+5"`, `"01.00"`, `true`, `null`, `[]`, `"` + strings.Repeat("9", 60) + `"`} {
		r := eval(t, c, fmt.Sprintf(`{"total":{"value":%s,"ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`, v))
		if r.OK() {
			t.Fatalf("%s was accepted: %s", v, r.Canonical)
		}
	}
}

func TestEnum_UnknownIsQuarantinedNotCoerced_NP09(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	r := eval(t, c, `{"total":{"value":"1","ccy":"XXX"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if r.OK() || r.Issues[0].Class != ClassMapping || r.Issues[0].Code != "enum_unmapped" {
		t.Fatalf("%v", codes(r))
	}
	// case-sensitive: "gbp" is not "GBP"
	r = eval(t, c, `{"total":{"value":"1","ccy":"gbp"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if r.OK() {
		t.Fatal("case-insensitive enum match")
	}
	// the contract may say otherwise, explicitly
	s := invoiceSpec()
	s.Fields[1].UnknownDefault = sp("OTHER")
	r = eval(t, mustCompile(t, s), `{"total":{"value":"1","ccy":"XXX"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if !r.OK() || !strings.Contains(string(r.Canonical), `"currency":"OTHER"`) {
		t.Fatalf("%v", codes(r))
	}
	if !r.Lineage[1].Defaulted {
		t.Fatal("an enum fallback must be marked defaulted in the lineage")
	}
}

func TestRequiredMissing_NeverInvented_NP11(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	for name, doc := range map[string]string{
		"absent": `{"total":{"value":"1","ccy":"GBP"},"supplier":{"id":"a"}}`,
		"null":   `{"total":{"value":"1","ccy":"GBP"},"issued":null,"supplier":{"id":"a"}}`,
	} {
		r := eval(t, c, doc)
		if r.OK() || r.Canonical != nil {
			t.Fatalf("%s: produced output", name)
		}
		if got := codes(r); len(got) != 1 || got[0] != "MAPPING:required_missing:/issued_at" {
			t.Fatalf("%s: %v", name, got)
		}
	}
	// whitespace-only is missing for a trimmed required string
	r := eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"   "}}`)
	if r.OK() {
		t.Fatal("blank required string accepted")
	}
}

func TestDatetime_ZonelessIsRefused_OffsetPreserved(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	for _, v := range []string{"2026-03-01T10:00:00", "2026-03-01 10:00:00Z", "2026-03-01", "not a time", "2026-13-01T10:00:00Z"} {
		r := eval(t, c, fmt.Sprintf(`{"total":{"value":"1","ccy":"GBP"},"issued":%q,"supplier":{"id":"a"}}`, v))
		if r.OK() {
			t.Fatalf("%q accepted", v)
		}
	}
	r := eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T23:30:00-05:00","supplier":{"id":"a"}}`)
	if !strings.Contains(string(r.Canonical), `"issued_at":"2026-03-02T04:30:00Z"`) ||
		!strings.Contains(string(r.Canonical), `"issued_at_source_offset":"-05:00"`) {
		t.Fatalf("%s", r.Canonical)
	}
	r = eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`)
	if !strings.Contains(string(r.Canonical), `"issued_at_source_offset":"+00:00"`) {
		t.Fatalf("%s", r.Canonical)
	}
}

func TestDate_Strict(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	for _, v := range []string{"2026-02-30", "2026-3-1", "01/04/2026", "2026-04-01T00:00:00Z", "20260401"} {
		r := eval(t, c, fmt.Sprintf(`{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","due":%q,"supplier":{"id":"a"}}`, v))
		if r.OK() {
			t.Fatalf("date %q accepted", v)
		}
	}
}

func TestTypeStrictness_NoCoercion(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	base := `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"},%s}`
	for _, extra := range []string{`"lines":"3"`, `"lines":3.5`, `"lines":1e2`, `"paid":"true"`, `"paid":1`} {
		if r := eval(t, c, fmt.Sprintf(base, extra)); r.OK() {
			t.Fatalf("%s coerced", extra)
		}
	}
	if r := eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":12345}}`); r.OK() {
		t.Fatal("a number was coerced to a string")
	}
	if r := eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"12345678901"}}`); r.OK() {
		t.Fatal("max_length not enforced")
	}
}

func TestProhibitedField_HeldBeforeAnythingIsMapped_NP12(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	r := eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"},"payer":{"national_id":"AB123456C"}}`)
	if r.OK() || r.Issues[0].Class != ClassDataPolicy || r.Issues[0].Code != "prohibited_field" || r.Canonical != nil || r.Lineage != nil {
		t.Fatalf("%v", codes(r))
	}
	// null is "not sent"
	r = eval(t, c, `{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"},"payer":{"national_id":null}}`)
	if !r.OK() {
		t.Fatalf("null prohibited field blocked: %v", codes(r))
	}
}

func TestIssuesNeverCarryValues(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	secret := "SUPER-SECRET-PAYLOAD-9f3a"
	for _, doc := range []string{
		fmt.Sprintf(`{"total":{"value":"%s","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`, secret),
		fmt.Sprintf(`{"total":{"value":"1","ccy":"%s"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"}}`, secret),
		fmt.Sprintf(`{"total":{"value":"1","ccy":"GBP"},"issued":"%s","supplier":{"id":"a"}}`, secret),
		fmt.Sprintf(`{"total":{"value":"1","ccy":"GBP"},"issued":"2026-03-01T10:00:00Z","supplier":{"id":"a"},"payer":{"national_id":"%s"}}`, secret),
	} {
		r := eval(t, c, doc)
		raw, _ := json.Marshal(r)
		if r.OK() || strings.Contains(string(raw), secret) {
			t.Fatalf("value leaked or accepted: %s", raw)
		}
	}
}

func TestNoPartialOutputOnAnyIssue(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	r := eval(t, c, `{"total":{"value":"1","ccy":"NOPE"},"issued":"bad","supplier":{"id":"a"}}`)
	if r.OK() || len(r.Issues) != 2 || r.Canonical != nil || r.CanonicalHash != "" || r.Lineage != nil {
		t.Fatalf("%v canonical=%s", codes(r), r.Canonical)
	}
}

func TestLineage_TracesEveryCanonicalField(t *testing.T) {
	r := eval(t, mustCompile(t, invoiceSpec()), goodDoc)
	got := map[string]Lineage{}
	for _, l := range r.Lineage {
		got[l.CanonicalField] = l
		if l.MappingVersionID != "mv-1" {
			t.Errorf("%s lacks the mapping version", l.CanonicalField)
		}
	}
	if got["/amount"].SourcePath != "/total/value" || got["/amount"].Rule != TypeDecimal {
		t.Fatalf("amount lineage %+v", got["/amount"])
	}
	if !got["/source"].Defaulted || got["/source"].Rule != "const" {
		t.Fatalf("const lineage %+v", got["/source"])
	}
	if len(r.Lineage) != 8 {
		t.Fatalf("%d lineage rows", len(r.Lineage))
	}
}

func TestSourceLimits(t *testing.T) {
	c := mustCompile(t, invoiceSpec())
	if r := c.Evaluate([]byte(goodDoc), "", 10); r.OK() || r.Issues[0].Code != "source_too_large" {
		t.Fatalf("%v", codes(r))
	}
	deep := strings.Repeat(`{"a":`, 100) + `1` + strings.Repeat(`}`, 100)
	if r := c.Evaluate([]byte(deep), "", 0); r.OK() || r.Issues[0].Code != "source_too_deep" {
		t.Fatalf("%v", codes(r))
	}
	for _, d := range []string{``, `{`, `[]`, `"x"`, `{"a":1} {"b":2}`, `null`} {
		if r := c.Evaluate([]byte(d), "", 0); r.OK() {
			t.Fatalf("%q accepted", d)
		}
	}
}

func TestArrayPointer(t *testing.T) {
	s := Spec{CanonicalContract: "x", Fields: []Field{{To: "/first", From: "/items/0/sku", Type: TypeString, Required: true}}}
	c := mustCompile(t, s)
	if r := eval(t, c, `{"items":[{"sku":"A"},{"sku":"B"}]}`); !r.OK() || string(r.Canonical) != `{"first":"A"}` {
		t.Fatalf("%v %s", codes(r), r.Canonical)
	}
	for _, d := range []string{`{"items":[]}`, `{"items":[{"sku":"A"}],"x":1}`[:0] + `{"items":{}}`, `{"items":[{"nosku":1}]}`} {
		if r := eval(t, c, d); r.OK() {
			t.Fatalf("%s accepted", d)
		}
	}
}

// ---- compile-time refusals: a field that is not enforced can never be silently ignored ------------------------

func TestCompileRefusals(t *testing.T) {
	cases := map[string]func(*Spec){
		"no contract":         func(s *Spec) { s.CanonicalContract = "" },
		"no fields":           func(s *Spec) { s.Fields = nil },
		"bad to pointer":      func(s *Spec) { s.Fields[0].To = "amount" },
		"duplicate to":        func(s *Spec) { s.Fields[1].To = s.Fields[0].To },
		"credential field":    func(s *Spec) { s.Fields[0].To = "/api_key" },
		"nested to":           func(s *Spec) { s.Fields[1].To = "/amount/inner" },
		"bad from pointer":    func(s *Spec) { s.Fields[0].From = "total.value" },
		"missing from":        func(s *Spec) { s.Fields[0].From = "" },
		"unknown type":        func(s *Spec) { s.Fields[0].Type = "float" },
		"decimal no scale":    func(s *Spec) { s.Fields[0].Scale = nil },
		"decimal scale huge":  func(s *Spec) { s.Fields[0].Scale = ip(40) },
		"enum empty":          func(s *Spec) { s.Fields[1].Enum = nil },
		"scale on string":     func(s *Spec) { s.Fields[4].Scale = ip(2) },
		"enum on string":      func(s *Spec) { s.Fields[4].Enum = map[string]string{"a": "b"} },
		"offset on date":      func(s *Spec) { s.Fields[3].KeepSourceOffset = true },
		"maxlen on integer":   func(s *Spec) { s.Fields[5].MaxLength = ip(3) },
		"maxlen zero":         func(s *Spec) { s.Fields[4].MaxLength = ip(0) },
		"bad prohibited":      func(s *Spec) { s.ProhibitedSourcePaths = []string{"payer"} },
		"default w/o declare": func(s *Spec) { s.Fields[7].NonBusinessDefault = false },
		"declare w/o default": func(s *Spec) { s.Fields[4].NonBusinessDefault = true },
		"const with source":   func(s *Spec) { s.Fields[7].From = "/x" },
		"both defaults":       func(s *Spec) { s.Fields[7].DefaultIfMissing = sp("a") },
		"default amount": func(s *Spec) {
			s.Fields[0].DefaultIfMissing = sp("0")
			s.Fields[0].NonBusinessDefault = true
			s.Fields[0].Required = false
		},
		"default date": func(s *Spec) { s.Fields[3].DefaultIfMissing = sp("2026-01-01"); s.Fields[3].NonBusinessDefault = true },
		"default enum": func(s *Spec) {
			s.Fields[1].DefaultIfMissing = sp("GBP")
			s.Fields[1].NonBusinessDefault = true
			s.Fields[1].Required = false
		},
		"default on required": func(s *Spec) { s.Fields[4].DefaultIfMissing = sp("n/a"); s.Fields[4].NonBusinessDefault = true },
		"bad boolean default": func(s *Spec) { s.Fields[6].DefaultIfMissing = sp("yes"); s.Fields[6].NonBusinessDefault = true },
	}
	for name, mutate := range cases {
		s := invoiceSpec()
		mutate(&s)
		if _, err := Compile(s); err == nil || !errors.Is(err, ErrSpecInvalid) {
			t.Errorf("%s: compiled (err=%v)", name, err)
		}
	}
}

func TestCompileJSON_RefusesUnknownKeysAndTrailing(t *testing.T) {
	good := `{"canonical_contract":"x","fields":[{"to":"/a","from":"/a","type":"string"}]}`
	if _, err := CompileJSON([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown key":   `{"canonical_contract":"x","fields":[{"to":"/a","from":"/a","type":"string","script":"1+1"}]}`,
		"unknown top":   `{"canonical_contract":"x","fields":[{"to":"/a","from":"/a","type":"string"}],"hook":"x"}`,
		"trailing":      good + `{}`,
		"empty":         ``,
		"not an object": `[]`,
	} {
		if _, err := CompileJSON([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHash_StableAndSensitive(t *testing.T) {
	a := mustCompile(t, invoiceSpec())
	b := mustCompile(t, invoiceSpec())
	if a.Hash() != b.Hash() || len(a.Hash()) != 64 {
		t.Fatal("hash unstable")
	}
	// the same spec written with a different layout hashes the same
	raw, _ := json.Marshal(invoiceSpec())
	var pretty map[string]any
	_ = json.Unmarshal(raw, &pretty)
	indented, _ := json.MarshalIndent(pretty, "", "      ")
	c, err := CompileJSON(indented)
	if err != nil || c.Hash() != a.Hash() {
		t.Fatalf("layout changed the hash (err=%v)", err)
	}
	// any change of meaning changes it
	s := invoiceSpec()
	s.Fields[0].Scale = ip(3)
	if mustCompile(t, s).Hash() == a.Hash() {
		t.Fatal("scale change did not change the hash")
	}
	s = invoiceSpec()
	s.Fields[1].Enum["USD"] = "GBP"
	if mustCompile(t, s).Hash() == a.Hash() {
		t.Fatal("enum change did not change the hash")
	}
}

func TestBooleanDefault(t *testing.T) {
	s := Spec{CanonicalContract: "x", Fields: []Field{
		{To: "/flag", From: "/flag", Type: TypeBoolean, DefaultIfMissing: sp("false"), NonBusinessDefault: true},
	}}
	c := mustCompile(t, s)
	if r := eval(t, c, `{}`); !r.OK() || string(r.Canonical) != `{"flag":false}` || !r.Lineage[0].Defaulted {
		t.Fatalf("%v %s", codes(r), r.Canonical)
	}
	if r := eval(t, c, `{"flag":true}`); string(r.Canonical) != `{"flag":true}` {
		t.Fatalf("%s", r.Canonical)
	}
}

func TestNoHTMLEscapingInCanonicalOutput(t *testing.T) {
	s := Spec{CanonicalContract: "x", Fields: []Field{{To: "/n", From: "/n", Type: TypeString, Required: true}}}
	r := eval(t, mustCompile(t, s), `{"n":"A & B <c>"}`)
	if string(r.Canonical) != `{"n":"A & B <c>"}` {
		t.Fatalf("%s", r.Canonical)
	}
}

func FuzzEvaluate(f *testing.F) {
	c, _ := Compile(invoiceSpec())
	f.Add([]byte(goodDoc))
	f.Add([]byte(`{"total":{"value":"1e3"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r := c.Evaluate(b, "mv", 1<<20)
		if r.OK() && len(r.Canonical) == 0 {
			t.Fatal("OK with no output")
		}
		if !r.OK() && r.Canonical != nil {
			t.Fatal("output alongside issues")
		}
		// whatever came in, an accepted payload is one valid JSON object and deterministic
		if r.OK() {
			var m map[string]any
			if err := json.Unmarshal(r.Canonical, &m); err != nil {
				t.Fatalf("canonical not JSON: %v", err)
			}
			if r2 := c.Evaluate(b, "mv", 1<<20); r2.CanonicalHash != r.CanonicalHash {
				t.Fatal("nondeterministic")
			}
		}
	})
}
