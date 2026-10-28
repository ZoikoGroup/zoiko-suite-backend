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
