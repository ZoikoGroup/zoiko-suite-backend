package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const vatSchema = `{
  "type": "object",
  "required": ["period", "boxes"],
  "additionalProperties": false,
  "properties": {
    "period": {"type": "string", "pattern": "^[0-9]{4}-(0[1-9]|1[0-2])$"},
    "taxpayer_ref": {"type": "string", "minLength": 3, "maxLength": 20},
    "kind": {"enum": ["VAT_RETURN", "EC_SALES"]},
    "boxes": {
      "type": "object",
      "required": ["vat_due"],
      "properties": {
        "vat_due": {"type": "number", "minimum": 0},
        "lines": {"type": "integer", "maximum": 1000}
      }
    },
    "items": {"type": "array", "items": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}}}}
  }
}`

func taxSpec() Spec {
	return Spec{
		Kind: "TAX", RequiredScope: "tax.file", Rails: []string{"VAT-RETURN@2"},
		Submit: Request{Path: "/v1/returns", Body: []Field{
			{Name: "return", From: "document"},
			{Name: "submission_ref", From: "canonical", Source: "reference"},
			{Name: "schema", From: "const", Const: "VAT-RETURN@2"},
		}},
		Response: Response{RefPath: "receipt_id", StatusPath: "status", EchoPath: "submission_ref",
			StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "REJECTED": "REJECTED"}},
		Auth: Auth{Type: "bearer"}, IdempotencyHeader: "Idempotency-Key", RetrySafe: true, TimeoutSeconds: 3,
		Schema: json.RawMessage(vatSchema), SchemaVersion: "2",
	}
}

func TestTaxProfileValidation(t *testing.T) {
	if err := taxSpec().Normalize().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Spec){
		"no schema":       func(s *Spec) { s.Schema = nil },
		"bad schema":      func(s *Spec) { s.Schema = json.RawMessage(`{"type":"nope"}`) },
		"unknown keyword": func(s *Spec) { s.Schema = json.RawMessage(`{"type":"object","oneOf":[]}`) },
		"bad pattern":     func(s *Spec) { s.Schema = json.RawMessage(`{"type":"string","pattern":"("}`) },
		"open additional": func(s *Spec) { s.Schema = json.RawMessage(`{"type":"object","additionalProperties":true}`) },
		"no document":     func(s *Spec) { s.Submit.Body = s.Submit.Body[1:] },
		"two documents":   func(s *Spec) { s.Submit.Body = append(s.Submit.Body, Field{Name: "again", From: "document"}) },
		"bad echo path":   func(s *Spec) { s.Response.EchoPath = "a b" },
	}
	for name, mut := range cases {
		s := taxSpec()
		mut(&s)
		if err := s.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
			t.Errorf("%s: want ErrSpecInvalid, got %v", name, err)
		}
	}
	// a bank profile cannot embed a document
	b := bankSpec()
	b.Submit.Body = append(b.Submit.Body, Field{Name: "doc", From: "document"})
	if err := b.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
		t.Errorf("bank + document: %v", err)
	}
}

func TestSchemaValidation(t *testing.T) {
	s := taxSpec().Normalize()
	c := Canonical{"rail": "VAT-RETURN@2", "reference": "sub-1"}
	good := `{"period":"2026-03","taxpayer_ref":"GB123456","kind":"VAT_RETURN","boxes":{"vat_due":125.50,"lines":12},"items":[{"id":"a"}]}`
	raw, err := s.BuildRequestDoc(c, []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"vat_due":125.50`) || !strings.Contains(string(raw), `"submission_ref":"sub-1"`) {
		t.Fatalf("exact number preserved and reference carried: %s", raw)
	}
	bad := map[string]string{
		"missing required": `{"period":"2026-03"}`,
		"wrong type":       `{"period":"2026-03","boxes":{"vat_due":"125"}}`,
		"pattern":          `{"period":"2026-13","boxes":{"vat_due":1}}`,
		"minimum":          `{"period":"2026-03","boxes":{"vat_due":-1}}`,
		"integer":          `{"period":"2026-03","boxes":{"vat_due":1,"lines":1.5}}`,
		"maximum":          `{"period":"2026-03","boxes":{"vat_due":1,"lines":1001}}`,
		"enum":             `{"period":"2026-03","kind":"OTHER","boxes":{"vat_due":1}}`,
		"minLength":        `{"period":"2026-03","taxpayer_ref":"ab","boxes":{"vat_due":1}}`,
		"maxLength":        `{"period":"2026-03","taxpayer_ref":"` + strings.Repeat("x", 21) + `","boxes":{"vat_due":1}}`,
		"additional":       `{"period":"2026-03","extra":1,"boxes":{"vat_due":1}}`,
		"nested item":      `{"period":"2026-03","boxes":{"vat_due":1},"items":[{"nope":1}]}`,
		"not json":         `nope`,
	}
	for name, doc := range bad {
		_, err := s.BuildRequestDoc(c, []byte(doc))
		if !errors.Is(err, ErrSchemaInvalid) {
			t.Errorf("%s: want ErrSchemaInvalid, got %v", name, err)
		}
	}
	// the failure names the path and the rule and never echoes a value
	_, err = s.BuildRequestDoc(c, []byte(`{"period":"SECRET-TAXPAYER-DATA","boxes":{"vat_due":1}}`))
	if err == nil || strings.Contains(err.Error(), "SECRET-TAXPAYER-DATA") || !strings.Contains(err.Error(), "$.period") {
		t.Fatalf("%v", err)
	}
	if _, err := s.BuildRequestDoc(c, nil); !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("no document: %v", err)
	}
	// unsupported schema/version: the rail is the authority schema version
	c["rail"] = "VAT-RETURN@9"
	if _, err := s.BuildRequestDoc(c, []byte(good)); !errors.Is(err, ErrRailUnsupported) {
		t.Fatalf("unsupported schema version: %v", err)
	}
}

func TestReceiptMustEchoOurReference(t *testing.T) {
	spec := taxSpec().Normalize()
	body, _ := spec.BuildRequestDoc(Canonical{"rail": "VAT-RETURN@2", "reference": "sub-1"}, []byte(`{"period":"2026-03","boxes":{"vat_due":1}}`))
	run := func(answer string) Result {
		tr, url, done := transport(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(answer)) })
		defer done()
		c := call(spec, url, body)
		c.EchoExpect = "sub-1"
		return tr.Submit(context.Background(), c)
	}
	if r := run(`{"receipt_id":"R1","status":"RECEIVED","submission_ref":"sub-1"}`); r.Class != ClassAcknowledged || r.ProviderRef != "R1" {
		t.Fatalf("%+v", r)
	}
	// a receipt for a different submission, or one that echoes nothing, is a mismatch, not an acceptance
	for name, a := range map[string]string{
		"other ref":  `{"receipt_id":"R1","status":"RECEIVED","submission_ref":"sub-2"}`,
		"no echo":    `{"receipt_id":"R1","status":"RECEIVED"}`,
		"empty echo": `{"receipt_id":"R1","status":"RECEIVED","submission_ref":""}`,
	} {
		if r := run(a); r.Class != ClassMismatch || r.ErrCode != "receipt_mismatch" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// without an expectation supplied an echo profile never acknowledges
	tr, url, done := transport(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"receipt_id":"R1","status":"RECEIVED","submission_ref":"sub-1"}`))
	})
	defer done()
	if r := tr.Submit(context.Background(), call(spec, url, body)); r.Class != ClassMismatch {
		t.Fatalf("%+v", r)
	}
}

func TestFilingMayCarryNoAmount(t *testing.T) {
	s := taxSpec().Normalize()
	if err := s.CheckAgainstProfile(Canonical{"rail": "VAT-RETURN@2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAgainstProfile(Canonical{"rail": "VAT-RETURN@2", "amount": "0"}); err != nil {
		t.Fatalf("a nil return has a zero tax amount: %v", err)
	}
	if err := s.CheckAgainstProfile(Canonical{"rail": "VAT-RETURN@2", "amount": "-1"}); err == nil {
		t.Fatal("a negative amount is not a filing amount")
	}
}
