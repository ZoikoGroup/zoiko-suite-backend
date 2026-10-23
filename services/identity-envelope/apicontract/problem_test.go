package apicontract

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteProblemFillsContractMembers(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/invoices", nil)
	r.Header.Set(HeaderRequestID, "req_0123456789abcdef")
	r.Header.Set(HeaderCorrelationID, "corr-1")

	p := ProblemFor(409, "stale_version", "nope")
	v := int64(41)
	p.CurrentVersion = &v
	p.Errors = []FieldError{{Pointer: "/line_items/0/quantity", Code: "invalid_range", Detail: "must be positive"}}
	WriteProblem(w, r, p)

	if got := w.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Fatalf("content type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache control = %q", got)
	}
	if got := w.Header().Get(HeaderRequestID); got != "req_0123456789abcdef" {
		t.Fatalf("request id header = %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != ProblemBase+"stale-version" {
		t.Fatalf("type = %v", body["type"])
	}
	if body["title"] != "Conflict" {
		t.Fatalf("title = %v", body["title"])
	}
	if body["request_id"] != "req_0123456789abcdef" || body["correlation_id"] != "corr-1" {
		t.Fatalf("ids = %v %v", body["request_id"], body["correlation_id"])
	}
	if body["retryable"] != false {
		t.Fatalf("retryable = %v", body["retryable"])
	}
	if body["current_version"] != float64(41) {
		t.Fatalf("current_version = %v", body["current_version"])
	}
	errs, ok := body["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors = %v", body["errors"])
	}
	if errs[0].(map[string]any)["pointer"] != "/line_items/0/quantity" {
		t.Fatalf("pointer = %v", errs[0])
	}
}

func TestWriteProblemExtraCannotOverrideContractFields(t *testing.T) {
	w := httptest.NewRecorder()
	p := ProblemFor(400, "validation_failed", "bad")
	p.Extra = map[string]any{"code": "spoofed", "retry_after_seconds": 30}
	WriteProblem(w, nil, p)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "validation_failed" {
		t.Fatalf("code overridden: %v", body["code"])
	}
	if body["retry_after_seconds"] != float64(30) {
		t.Fatalf("extra missing: %v", body["retry_after_seconds"])
	}
}

func TestProblemError(t *testing.T) {
	if got := (Problem{Code: "x", Detail: "y"}).Error(); got != "x: y" {
		t.Fatalf("error = %q", got)
	}
	if got := (Problem{Code: "x"}).Error(); got != "x" {
		t.Fatalf("error = %q", got)
	}
}
