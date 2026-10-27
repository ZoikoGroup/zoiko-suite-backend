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

// RetryAfter is a header, never a body member, and only on a retryable
// problem; DecisionID is both (gateway-auth-svc's admission refusals).
func TestWriteProblemRetryAfterAndDecisionID(t *testing.T) {
	w := httptest.NewRecorder()
	p := ProblemFor(503, "dependency_unavailable", "")
	p.Retryable, p.RetryAfter, p.DecisionID = true, 5, "dec-1"
	WriteProblem(w, httptest.NewRequest("GET", "/verify", nil), p)
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q", got)
	}
	if got := w.Header().Get("X-Gateway-Decision-Id"); got != "dec-1" {
		t.Fatalf("decision id header = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, leaked := body["retry_after"]; leaked || body["decision_id"] != "dec-1" {
		t.Fatalf("body members wrong: %v", body)
	}

	w = httptest.NewRecorder()
	p = ProblemFor(400, "invalid", "")
	p.RetryAfter = 5
	WriteProblem(w, httptest.NewRequest("GET", "/verify", nil), p)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After on a non-retryable problem = %q", got)
	}
}

// 429, 503 and 504 carry the retry hint even when the caller built the
// problem as a struct literal; anything else keeps what the caller set.
func TestWriteProblemMarksTransientStatusesRetryable(t *testing.T) {
	for status, want := range map[int]bool{429: true, 503: true, 504: true, 500: false, 409: false} {
		w := httptest.NewRecorder()
		WriteProblem(w, httptest.NewRequest("GET", "/v1/x", nil), Problem{Status: status, Code: "c"})
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["retryable"] != want {
			t.Fatalf("status %d: retryable = %v, want %v", status, body["retryable"], want)
		}
	}
}
