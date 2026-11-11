package schemainfer

import (
	"encoding/json"
	"strings"
	"testing"

	"zoiko.io/eventing/integration"
)

func samples(js ...string) []json.RawMessage {
	var out []json.RawMessage
	for _, j := range js {
		out = append(out, json.RawMessage(j))
	}
	return out
}

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, b)
	}
	return m
}

func TestRequiredOnlyWhenPresentInEverySample(t *testing.T) {
	b, err := Infer(samples(`{"id":"a","note":"x"}`, `{"id":"b"}`, `{"id":"c","note":"y"}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, b)
	req := m["required"].([]any)
	if len(req) != 1 || req[0] != "id" {
		t.Fatalf("required = %v, want only id", req)
	}
	props := m["properties"].(map[string]any)
	if props["note"].(map[string]any)["type"] != "string" {
		t.Fatal("an optional field still gets its type")
	}
}

func TestOneSampleProvesNothingAboutRequired(t *testing.T) {
	b, _ := Infer(samples(`{"id":"a"}`), Options{})
	if _, has := parse(t, b)["required"]; has {
		t.Fatal("a single sample must not make a field required")
	}
	b, _ = Infer(samples(`{"id":"a"}`), Options{MinSamples: 1})
	if _, has := parse(t, b)["required"]; !has {
		t.Fatal("MinSamples 1 states required")
	}
}

func TestTypesAreStatedOnlyWhenAllSamplesAgree(t *testing.T) {
	b, _ := Infer(samples(`{"n":1,"f":1.5,"m":1,"z":null,"x":"a"}`, `{"n":2,"f":2,"m":"two","z":null,"x":"b"}`), Options{})
	p := parse(t, b)["properties"].(map[string]any)
	typ := func(k string) any { return p[k].(map[string]any)["type"] }
	if typ("n") != "integer" {
		t.Errorf("n = %v", typ("n"))
	}
	if typ("f") != "number" {
		t.Errorf("a field with a fractional sample is number, got %v", typ("f"))
	}
	if typ("m") != nil {
		t.Errorf("mixed types must state none, got %v", typ("m"))
	}
	if typ("z") != nil {
		t.Errorf("null must state none, got %v", typ("z"))
	}
	if typ("x") != "string" {
		t.Errorf("x = %v", typ("x"))
	}
}

func TestDateTimeFormatOnlyWhenEveryStringParses(t *testing.T) {
	b, _ := Infer(samples(`{"a":"2026-03-01T10:00:00Z","b":"2026-03-01T10:00:00Z"}`, `{"a":"2026-03-02T10:00:00Z","b":"not a date"}`), Options{})
	p := parse(t, b)["properties"].(map[string]any)
	if p["a"].(map[string]any)["format"] != "date-time" {
		t.Error("a is always a timestamp")
	}
	if _, has := p["b"].(map[string]any)["format"]; has {
		t.Error("b had a non-date sample")
	}
}

func TestNestedObjectsAndArrays(t *testing.T) {
	b, _ := Infer(samples(`{"o":{"k":"v"},"l":[{"id":1},{"id":2}]}`, `{"o":{"k":"w"},"l":[{"id":3}]}`), Options{})
	p := parse(t, b)["properties"].(map[string]any)
	o := p["o"].(map[string]any)
	if o["type"] != "object" || o["required"].([]any)[0] != "k" {
		t.Errorf("nested object: %v", o)
	}
	l := p["l"].(map[string]any)
	items := l["items"].(map[string]any)
	if l["type"] != "array" || items["type"] != "object" || items["properties"].(map[string]any)["id"].(map[string]any)["type"] != "integer" {
		t.Errorf("array items: %v", l)
	}
}

func TestInferredSchemaAcceptsItsSamplesAndRejectsBreakage(t *testing.T) {
	smp := samples(`{"id":"a","qty":1,"at":"2026-03-01T10:00:00Z"}`, `{"id":"b","qty":2,"at":"2026-03-02T10:00:00Z"}`)
	b, err := Infer(smp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range smp {
		if v := integration.ValidatePayload(b, s); len(v) != 0 {
			t.Fatalf("an inferred schema must accept its own samples: %v", v)
		}
	}
	if v := integration.ValidatePayload(b, json.RawMessage(`{"qty":1}`)); len(v) == 0 {
		t.Error("a missing required field must be rejected")
	}
	if v := integration.ValidatePayload(b, json.RawMessage(`{"id":"a","qty":"1","at":"x"}`)); len(v) == 0 {
		t.Error("a wrong type must be rejected")
	}
}

func TestRefusals(t *testing.T) {
	if _, err := Infer(nil, Options{}); err == nil {
		t.Error("no samples")
	}
	if _, err := Infer(samples(`[1]`), Options{}); err == nil {
		t.Error("a non-object sample")
	}
	if _, err := Infer(samples(`{`), Options{}); err == nil {
		t.Error("malformed JSON")
	}
}

func TestDescriptionStatesItIsObserved(t *testing.T) {
	b, _ := Infer(samples(`{"a":1}`, `{"a":2}`), Options{Description: "X."})
	d := parse(t, b)["description"].(string)
	if !strings.HasPrefix(d, "X.") || !strings.Contains(d, "OBSERVED") {
		t.Fatalf("description = %q", d)
	}
}
