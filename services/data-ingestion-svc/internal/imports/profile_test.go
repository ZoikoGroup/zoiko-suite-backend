package imports

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
)

func baseSpec() Spec {
	return Spec{
		ImportType: "supplier-invoices", Format: "csv",
		Columns: []Column{
			{Name: "invoice_no", Type: TypeString, Required: true, MaxLen: 20, Pattern: `^INV-[0-9]+$`},
			{Name: "entity", Type: TypeString, Required: true},
			{Name: "amount", Type: TypeDecimal, Required: true, NonNegative: true},
			{Name: "qty", Type: TypeInt},
			{Name: "due", Type: TypeDate},
			{Name: "paid", Type: TypeBool},
			{Name: "kind", Type: TypeEnum, Enum: []string{"goods", "services"}},
		},
		KeyColumns: []string{"invoice_no"}, EntityColumn: "entity",
		Control: &Control{Column: "amount", Required: true},
		Target:  Target{Key: "ap", Method: "POST", Path: "/v1/invoices", ResultRefField: "id"},
		Mapping: []MapRule{
			{Field: "number", From: "column", Column: "invoice_no"}, {Field: "amount", From: "column", Column: "amount"},
			{Field: "legal_entity_id", From: "legal_entity_id"}, {Field: "origin", From: "const", Const: "import"},
		},
	}.Normalize()
}

func TestValidSpecPasses(t *testing.T) {
	if err := baseSpec().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSpecValidationRefusesUnsafeOrIncompleteProfiles(t *testing.T) {
	for name, mut := range map[string]func(*Spec){
		"no import type":                  func(s *Spec) { s.ImportType = "" },
		"unknown format":                  func(s *Spec) { s.Format = "xlsx" },
		"multi-char delimiter":            func(s *Spec) { s.Delimiter = ";;" },
		"unknown partial policy":          func(s *Spec) { s.PartialPolicy = "YOLO" },
		"no columns":                      func(s *Spec) { s.Columns = nil },
		"repeated column":                 func(s *Spec) { s.Columns = append(s.Columns, s.Columns[0]) },
		"bad column name":                 func(s *Spec) { s.Columns[0].Name = "1 bad" },
		"unknown type":                    func(s *Spec) { s.Columns[0].Type = "blob" },
		"enum without values":             func(s *Spec) { s.Columns[6].Enum = nil },
		"pattern on a number":             func(s *Spec) { s.Columns[2].Pattern = "^1$" },
		"bad regex":                       func(s *Spec) { s.Columns[0].Pattern = "(" },
		"key is not a column":             func(s *Spec) { s.KeyColumns = []string{"nope"} },
		"entity column not str":           func(s *Spec) { s.EntityColumn = "amount" },
		"control not numeric":             func(s *Spec) { s.Control.Column = "invoice_no" },
		"no target key":                   func(s *Spec) { s.Target.Key = "" },
		"bad method":                      func(s *Spec) { s.Target.Method = "DELETE" },
		"absolute url as path":            func(s *Spec) { s.Target.Path = "http://evil.example/x" },
		"relative path":                   func(s *Spec) { s.Target.Path = "v1/invoices" },
		"query in path":                   func(s *Spec) { s.Target.Path = "/v1/invoices?x=1" },
		"traversal in path":               func(s *Spec) { s.Target.Path = "/v1/../admin" },
		"non-2xx success code":            func(s *Spec) { s.Target.SuccessCodes = []int{200, 500} },
		"no mapping":                      func(s *Spec) { s.Mapping = nil },
		"mapping to missing col":          func(s *Spec) { s.Mapping[0].Column = "nope" },
		"unknown mapping source":          func(s *Spec) { s.Mapping[0].From = "env" },
		"const without a value":           func(s *Spec) { s.Mapping[3].Const = nil },
		"result ref required but unnamed": func(s *Spec) { s.RequireResultRef = true; s.Target.ResultRefField = "" },
	} {
		s := baseSpec()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A material profile cannot opt out of the controls that make a financial import safe.
func TestMaterialProfilesCannotWeakenTheirControls(t *testing.T) {
	m := func(mut func(*Spec)) error {
		s := baseSpec()
		s.Material, s.RequireResultRef = true, true
		mut(&s)
		return s.Validate()
	}
	if err := m(func(*Spec) {}); err != nil {
		t.Fatalf("a well-formed material profile: %v", err)
	}
	for name, mut := range map[string]func(*Spec){
		"skips invalid rows":   func(s *Spec) { s.PartialPolicy = PartialSkipInvalid },
		"no control total":     func(s *Spec) { s.Control = nil },
		"control not required": func(s *Spec) { s.Control.Required = false },
		"no result reference":  func(s *Spec) { s.RequireResultRef = false },
		"result field unnamed": func(s *Spec) { s.Target.ResultRefField = "" },
	} {
		if err := m(mut); err == nil {
			t.Errorf("a material profile that %s was accepted", name)
		}
	}
}

func TestSpecHashesMoveWithTheirMeaning(t *testing.T) {
	a, b := baseSpec(), baseSpec()
	if a.Hash() != b.Hash() || a.MappingHash() != b.MappingHash() {
		t.Fatal("hashes must be deterministic")
	}
	b.Mapping[1].Column = "qty"
	if a.MappingHash() == b.MappingHash() || a.Hash() == b.Hash() {
		t.Fatal("changing the mapping must change both hashes")
	}
	c := baseSpec()
	c.MaxRows = 5
	if a.MappingHash() != c.MappingHash() {
		t.Fatal("a row limit does not change what an item is turned into")
	}
	if a.Hash() == c.Hash() {
		t.Fatal("but it is part of the specification")
	}
}

func validRaw() map[string]any {
	return map[string]any{"invoice_no": "INV-1", "entity": "le-1", "amount": "10.50", "qty": "3", "due": "2026-10-17", "paid": "true", "kind": "goods"}
}

func TestValidateRowTypesAndConverts(t *testing.T) {
	v, errs := baseSpec().ValidateRow(validRaw(), "le-1")
	if len(errs) != 0 {
		t.Fatalf("%+v", errs)
	}
	if v["amount"] != "10.5" || v["qty"] != int64(3) || v["due"] != "2026-10-17" || v["paid"] != true || v["invoice_no"] != "INV-1" {
		t.Fatalf("%#v", v)
	}
}

func codes(errs []RowError) string {
	var s []string
	for _, e := range errs {
		s = append(s, e.Column+":"+e.Code)
	}
	return strings.Join(s, ",")
}

func TestValidateRowReportsEveryProblemWithoutEchoingValues(t *testing.T) {
	secret := "SECRET-VALUE-9999"
	cases := map[string]struct {
		mut  func(map[string]any)
		want string
	}{
		"missing required":  {func(m map[string]any) { delete(m, "amount") }, "amount:REQUIRED"},
		"blank required":    {func(m map[string]any) { m["invoice_no"] = "  " }, "invoice_no:REQUIRED"},
		"too long":          {func(m map[string]any) { m["invoice_no"] = "INV-" + strings.Repeat("9", 30) }, "invoice_no:MAX_LEN"},
		"pattern":           {func(m map[string]any) { m["invoice_no"] = secret }, "invoice_no:PATTERN"},
		"decimal garbage":   {func(m map[string]any) { m["amount"] = secret }, "amount:TYPE"},
		"decimal exponent":  {func(m map[string]any) { m["amount"] = "1e3" }, "amount:TYPE"},
		"too many decimals": {func(m map[string]any) { m["amount"] = "1.1234567" }, "amount:TYPE"},
		"negative amount":   {func(m map[string]any) { m["amount"] = "-1" }, "amount:NEGATIVE_NOT_ALLOWED"},
		"int garbage":       {func(m map[string]any) { m["qty"] = "3.5" }, "qty:TYPE"},
		"bad date":          {func(m map[string]any) { m["due"] = "17/10/2026" }, "due:TYPE"},
		"impossible date":   {func(m map[string]any) { m["due"] = "2026-02-30" }, "due:TYPE"},
		"bool garbage":      {func(m map[string]any) { m["paid"] = "yes" }, "paid:TYPE"},
		"enum":              {func(m map[string]any) { m["kind"] = secret }, "kind:ENUM"},
		"cross entity":      {func(m map[string]any) { m["entity"] = "le-OTHER" }, "entity:CROSS_ENTITY"},
		"unknown field":     {func(m map[string]any) { m["surprise"] = secret }, "surprise:UNKNOWN_FIELD"},
	}
	for name, c := range cases {
		raw := validRaw()
		c.mut(raw)
		_, errs := baseSpec().ValidateRow(raw, "le-1")
		if codes(errs) != c.want {
			t.Errorf("%s: got %q, want %q", name, codes(errs), c.want)
		}
		for _, e := range errs {
			if strings.Contains(e.Column+e.Code, secret) {
				t.Errorf("%s: an error echoes the value", name)
			}
		}
	}
	// several problems in one row are all reported
	raw := validRaw()
	delete(raw, "amount")
	raw["qty"] = "x"
	raw["entity"] = "le-OTHER"
	_, errs := baseSpec().ValidateRow(raw, "le-1")
	if len(errs) != 3 {
		t.Fatalf("%q", codes(errs))
	}
}

func TestValidateRowOptionalBlanksAreOmitted(t *testing.T) {
	raw := validRaw()
	raw["qty"], raw["due"], raw["kind"] = "", "  ", nil
	v, errs := baseSpec().ValidateRow(raw, "le-1")
	if len(errs) != 0 {
		t.Fatal(codes(errs))
	}
	for _, k := range []string{"qty", "due", "kind"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s should be omitted", k)
		}
	}
}

func TestDecimalsAreExactNeverFloats(t *testing.T) {
	sum := new(big.Rat)
	for i := 0; i < 1000; i++ {
		r, err := ParseDecimal("0.1")
		if err != nil {
			t.Fatal(err)
		}
		sum.Add(sum, r)
	}
	if CanonicalDecimal(sum) != "100" {
		t.Fatalf("1000 x 0.1 = %s (float arithmetic would give 99.9999...)", CanonicalDecimal(sum))
	}
	for in, want := range map[string]string{"10": "10", "10.50": "10.5", "0.000001": "0.000001", "-0": "0", "-1.5": "-1.5", "007": "7"} {
		r, err := ParseDecimal(in)
		if err != nil || CanonicalDecimal(r) != want {
			t.Errorf("%s -> %v %v, want %s", in, r, err, want)
		}
	}
	for _, bad := range []string{"", "1e3", "NaN", "1,5", ".5", "5.", "1.1234567", "12345678901234567890", "0x10", "--1"} {
		if _, err := ParseDecimal(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestItemKeyIsDeterministicAndRowSpecific(t *testing.T) {
	a := ItemKey("imb_abc", 1, RowHash(map[string]any{"a": 1}))
	if a != ItemKey("imb_abc", 1, RowHash(map[string]any{"a": 1})) {
		t.Fatal("not deterministic")
	}
	if a == ItemKey("imb_abc", 2, RowHash(map[string]any{"a": 1})) || a == ItemKey("imb_abd", 1, RowHash(map[string]any{"a": 1})) ||
		a == ItemKey("imb_abc", 1, RowHash(map[string]any{"a": 2})) {
		t.Fatal("distinct rows must have distinct keys")
	}
	if len(a) > 128 {
		t.Fatal("too long for the column")
	}
}

func TestBuildBodyAndExtractRef(t *testing.T) {
	s := baseSpec()
	body, err := s.BuildBody(map[string]any{"invoice_no": "INV-1", "amount": "10.5", "qty": int64(3)}, "t1", "le-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"amount":"10.5","legal_entity_id":"le-1","number":"INV-1","origin":"import"}` {
		t.Fatalf("%s", body)
	}
	if strings.Contains(string(body), "qty") {
		t.Fatal("only mapped fields are sent")
	}
	for in, want := range map[string]string{`{"id":"abc"}`: "abc", `{"a":{"b":"x"}}`: "", `{"id":42}`: "42", `nope`: "", `{"id":null}`: ""} {
		if got := ExtractRef("id", []byte(in)); got != want {
			t.Errorf("%s -> %q want %q", in, got, want)
		}
	}
	if ExtractRef("a.b", []byte(`{"a":{"b":"x"}}`)) != "x" || ExtractRef("", []byte(`{}`)) != "" {
		t.Error("dotted path")
	}
}

func TestKeyOf(t *testing.T) {
	s := baseSpec()
	if s.KeyOf(map[string]any{"invoice_no": "INV-1"}) == "" || s.KeyOf(map[string]any{}) != "" {
		t.Fatal("key")
	}
	s.KeyColumns = nil
	if s.KeyOf(map[string]any{"invoice_no": "INV-1"}) != "" {
		t.Fatal("no key columns means no duplicate detection")
	}
}

// ── parsing ─────────────────────────────────────────────────────────────────

func readAll(t *testing.T, s Spec, content string) ([]Row, error) {
	t.Helper()
	rr, err := NewRowReader(s, strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	var rows []Row
	for {
		r, err := rr.Next()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		rows = append(rows, r)
	}
}

func TestCSVParsing(t *testing.T) {
	s := baseSpec()
	rows, err := readAll(t, s, "\xEF\xBB\xBFinvoice_no,entity,amount\r\nINV-1,le-1,10\r\nINV-2,le-1,\"1,234.5\"\r\n")
	if err != nil || len(rows) != 2 || rows[0].No != 1 || rows[1].No != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	if rows[0].Raw["invoice_no"] != "INV-1" || rows[1].Raw["amount"] != "1,234.5" {
		t.Fatalf("%+v", rows)
	}
	// short and long rows, and a bare quote, are reported rows, not dropped ones
	rows, err = readAll(t, s, "invoice_no,entity,amount\nINV-1,le-1\nINV-2,le-1,5,extra\nINV-3,le-1,5\nINV-4,\"bad\"quote,5\n")
	if err != nil || len(rows) != 4 {
		t.Fatalf("%+v %v", rows, err)
	}
	if rows[0].Errs[0].Code != CodeFieldCount || rows[1].Errs[0].Code != CodeFieldCount || rows[2].Raw == nil || rows[3].Errs[0].Code != CodeMalformedLine {
		t.Fatalf("%+v", rows)
	}
	// a different delimiter
	s.Delimiter = ";"
	rows, err = readAll(t, s, "invoice_no;entity;amount\nINV-1;le-1;10\n")
	if err != nil || len(rows) != 1 || rows[0].Raw["amount"] != "10" {
		t.Fatalf("%+v %v", rows, err)
	}
}

// INT-NP: schema mismatch blocks before preview.
func TestCSVSchemaMismatchIsAFileLevelError(t *testing.T) {
	s := baseSpec()
	for name, content := range map[string]string{
		"empty file":         "",
		"missing required":   "invoice_no,entity\nINV-1,le-1\n",
		"unknown column":     "invoice_no,entity,amount,surprise\nINV-1,le-1,1,x\n",
		"repeated column":    "invoice_no,entity,amount,amount\nINV-1,le-1,1,1\n",
		"wrong header names": "no,ent,amt\n1,2,3\n",
	} {
		if _, err := readAll(t, s, content); !errors.Is(err, ErrSchemaMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// optional columns may be absent
	if _, err := readAll(t, s, "invoice_no,entity,amount\nINV-1,le-1,1\n"); err != nil {
		t.Errorf("optional columns absent: %v", err)
	}
	// a hostile header is not echoed in full or with control characters
	_, err := readAll(t, s, "invoice_no,entity,amount,"+strings.Repeat("A", 500)+"\x01\n")
	if err == nil || strings.Contains(err.Error(), strings.Repeat("A", 100)) || strings.Contains(err.Error(), "\x01") {
		t.Errorf("%v", err)
	}
}

func TestJSONLParsing(t *testing.T) {
	s := baseSpec()
	s.Format = "jsonl"
	rows, err := readAll(t, s, "{\"invoice_no\":\"INV-1\",\"amount\":10.5,\"qty\":3}\n\n   \n{\"invoice_no\":\"INV-2\"}\nnot json\n[1]\n{\"a\":1}{\"b\":2}\n")
	if err != nil || len(rows) != 5 {
		t.Fatalf("%d rows %v", len(rows), err)
	}
	if rows[0].No != 1 || rows[1].No != 2 || rows[2].Errs[0].Code != CodeMalformedLine || rows[3].Errs[0].Code != CodeMalformedLine || rows[4].Errs[0].Code != CodeMalformedLine {
		t.Fatalf("%+v", rows)
	}
	// numbers stay exact through to validation
	raw := rows[0].Raw
	raw["entity"] = "le-1"
	v, errs := s.ValidateRow(raw, "le-1")
	if len(errs) != 0 || v["amount"] != "10.5" || v["qty"] != int64(3) {
		t.Fatalf("%v %v", v, codes(errs))
	}
	// a float with an exponent is not a decimal; a JSON string where a number is wanted still parses as text
	raw2 := map[string]any{"invoice_no": "INV-1", "entity": "le-1", "amount": jsonNumber("1e3")}
	if _, errs := s.ValidateRow(raw2, "le-1"); codes(errs) != "amount:TYPE" {
		t.Fatalf("%q", codes(errs))
	}
	// an over-long line is a file-level failure, not an unbounded buffer
	if _, err := readAll(t, s, "{\"invoice_no\":\""+strings.Repeat("a", maxJSONLLine+10)+"\"}\n"); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("%v", err)
	}
}

func jsonNumber(s string) json.Number { return json.Number(s) }
