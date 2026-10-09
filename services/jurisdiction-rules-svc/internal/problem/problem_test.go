package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProblem_ContractShape pins the RFC 9457 body every non-2xx response
// carries: type/title/status/detail/code plus retryable, with the legacy
// `error` alias of `code` during the migration (API standard §12, GCP §16).
func TestProblem_ContractShape(t *testing.T) {
	rr := httptest.NewRecorder()
	Write(rr, New(http.StatusForbidden, "segregation_of_duties", "cannot attest to or approve their own rollout"))

	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"type":     "https://api.zoikosuite.com/problems/sod-conflict",
		"code":     "segregation_of_duties",
		"error":    "segregation_of_duties",
		"status":   float64(403),
		"retryable": false,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if body["detail"] != "cannot attest to or approve their own rollout" {
		t.Errorf("detail = %v", body["detail"])
	}
}

func TestProblem_RetryableTransient(t *testing.T) {
	if p := New(http.StatusServiceUnavailable, "store_unavailable", ""); !p.Retryable {
		t.Error("expected a 503 to carry retryable advisory")
	}
	if p := New(http.StatusConflict, "conflict", ""); p.Retryable {
		t.Error("expected a 409 to not be flagged retryable")
	}
}

func TestProblem_ExtensionsAndFieldErrors(t *testing.T) {
	p := New(http.StatusConflict, "operation_blocked", "one or more gates are not met").
		With("reasons", []string{"not_approved"}).
		Field("#/pack_id", "missing", "pack_id is required")

	rr := httptest.NewRecorder()
	Write(rr, p)
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := body["reasons"].([]any); !ok || len(got) != 1 || got[0] != "not_approved" {
		t.Errorf("reasons extension = %v", body["reasons"])
	}
	errs, ok := body["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors = %v", body["errors"])
	}
	first, ok := errs[0].(map[string]any)
	if !ok || first["pointer"] != "#/pack_id" || first["code"] != "missing" {
		t.Errorf("errors[0] = %v", first)
	}
}

func TestProblem_TypeFallbackByStatus(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   string
	}{
		{http.StatusNotFound, "rule_not_found", "not-found"},
		{http.StatusBadRequest, "invalid_request_body", "validation"},
		{http.StatusConflict, "invalid_transition", "concurrency-conflict"},
		{http.StatusUnauthorized, "missing_principal", "authorization-denied"},
	}
	for _, c := range cases {
		if got := New(c.status, c.code, "").Type; got != "https://api.zoikosuite.com/problems/"+c.want {
			t.Errorf("New(%d,%q).Type = %q, want .../problems/%s", c.status, c.code, got, c.want)
		}
	}
}