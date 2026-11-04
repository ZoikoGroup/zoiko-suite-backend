package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProblemFormat(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "req_abc")
	Write(w, httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", nil), TurnConflict, "")

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "publicai.turn_conflict" || p.RequestID != "req_abc" || !p.Retryable || p.Status != 409 {
		t.Fatalf("unexpected problem: %+v", p)
	}
	if p.Instance != "/public-ai/errors/req_abc" {
		t.Fatalf("instance = %q", p.Instance)
	}
}

func TestUnknownCodeFallsBackToInternal(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, httptest.NewRequest(http.MethodGet, "/", nil), Code("bogus"), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRFC9457ContractFields(t *testing.T) {
	cases := []struct {
		code      Code
		status    int
		retryable bool
	}{
		{InvalidRequest, http.StatusBadRequest, false},
		{TurnConflict, http.StatusConflict, true},
		{RateLimited, http.StatusTooManyRequests, true},
		{ServiceUnavailable, http.StatusServiceUnavailable, true},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-ID", "req_test")
		Write(w, httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", nil), c.code, "")
		if w.Code != c.status {
			t.Fatalf("%s: status = %d, want %d", c.code, w.Code, c.status)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("%s: content-type = %q", c.code, ct)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: missing no-store", c.code)
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s: %v", c.code, err)
		}
		if p.Type != TypeBase+string(c.code) {
			t.Fatalf("%s: type = %q", c.code, p.Type)
		}
		if p.Title == "" || p.Status != c.status || p.Code != "publicai."+string(c.code) {
			t.Fatalf("%s: bad fields %+v", c.code, p)
		}
		if p.RequestID != "req_test" {
			t.Fatalf("%s: request_id = %q", c.code, p.RequestID)
		}
		if p.Instance != "/public-ai/errors/req_test" {
			t.Fatalf("%s: instance = %q", c.code, p.Instance)
		}
		if p.Retryable != c.retryable {
			t.Fatalf("%s: retryable = %v want %v", c.code, p.Retryable, c.retryable)
		}
	}
}
